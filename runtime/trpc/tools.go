package trpcruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
	"github.com/lookerjin/grape-core/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/file"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

type runState struct {
	mu      sync.Mutex
	events  []agent.Event
	runID   string
	session string
	mode    agent.Mode
}

type stateKey struct{}
type workspaceKey struct{}

func withState(ctx context.Context, st *runState) context.Context {
	return context.WithValue(ctx, stateKey{}, st)
}

func stateFrom(ctx context.Context) *runState {
	st, _ := ctx.Value(stateKey{}).(*runState)
	return st
}

func (st *runState) add(evt agent.Event) {
	if st == nil {
		return
	}
	st.mu.Lock()
	st.events = append(st.events, evt)
	st.mu.Unlock()
}

func (st *runState) drain() []agent.Event {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	out := st.events
	st.events = nil
	return out
}

type cmdIn struct {
	Argv       []string `json:"argv"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

type cmdOut struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Isolation string `json:"isolation"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

type pathIn struct {
	Path string `json:"path,omitempty"`
}

func (rt *Runtime) workspaceTools(workspace string, allowWrite bool) ([]tool.Tool, error) {
	set, err := file.NewToolSet(
		file.WithBaseDir(workspace),
		file.WithSaveFileEnabled(allowWrite),
		file.WithReplaceContentEnabled(allowWrite),
	)
	if err != nil {
		return nil, err
	}
	var tools []tool.Tool
	for _, tl := range set.Tools(context.Background()) {
		name := ""
		if tl.Declaration() != nil {
			name = tl.Declaration().Name
		}
		if name == "save_file" || name == "replace_content" {
			callable, ok := tl.(tool.CallableTool)
			if !ok {
				return nil, fmt.Errorf("write tool %s is not callable", name)
			}
			tools = append(tools, &gatedTool{inner: callable, kind: "write", rt: rt})
			continue
		}
		tools = append(tools, tl)
	}
	tools = append(tools,
		function.NewFunctionTool(
			rt.runCommand,
			function.WithName("run_command"),
			function.WithDescription("Run an argv command in the workspace. argv is not a shell string. The result includes the isolation level that was actually used."),
		),
		function.NewFunctionTool(
			func(ctx context.Context, in pathIn) (map[string]string, error) {
				text, err := coding.GitStatus(ctx, workspace)
				if err != nil {
					return nil, err
				}
				return map[string]string{"status": text}, nil
			},
			function.WithName("git_status"),
			function.WithDescription("Read-only git status. Does not add, commit, push, reset, or clean."),
		),
		function.NewFunctionTool(
			func(ctx context.Context, in pathIn) (map[string]string, error) {
				text, err := coding.GitDiff(ctx, workspace)
				if err != nil {
					return nil, err
				}
				return map[string]string{"diff": text}, nil
			},
			function.WithName("git_diff"),
			function.WithDescription("Read-only git diff. Does not add, commit, push, reset, or clean."),
		),
	)
	return tools, nil
}

func (rt *Runtime) runCommand(ctx context.Context, in cmdIn) (cmdOut, error) {
	detail := strings.Join(in.Argv, " ")
	if err := rt.authorize(ctx, "command", detail, ""); err != nil {
		return cmdOut{}, err
	}
	ws := rt.cfg.Workspace.Root
	if v, ok := ctx.Value(workspaceKey{}).(string); ok && v != "" {
		ws = v
	}
	res, err := rt.exec.Execute(ctx, sandbox.Request{
		Workspace: ws,
		Argv:      in.Argv,
		Timeout:   time.Duration(in.TimeoutSec) * time.Second,
	})
	out := cmdOut{
		Stdout:    res.Stdout,
		Stderr:    res.Stderr,
		ExitCode:  res.ExitCode,
		Isolation: res.Isolation,
		TimedOut:  res.TimedOut,
	}
	return out, err
}

func (rt *Runtime) authorize(ctx context.Context, kind, detail, path string) error {
	if rt.cfg.Workspace.Permission == agent.PermissionReadOnly && (kind == "command" || kind == "write") {
		rt.recordDecision(ctx, kind, detail, path, false, "read_only")
		return agent.Wrap(agent.ErrPermissionDenied, kind, fmt.Errorf("read_only workspace"))
	}
	if !rt.needsApproval(ctx) {
		return nil
	}
	if rt.decider == nil {
		rt.recordDecision(ctx, kind, detail, path, false, "no decider")
		return agent.Wrap(agent.ErrPermissionDenied, kind, fmt.Errorf("approval required"))
	}
	st := stateFrom(ctx)
	req := agent.ApprovalRequest{
		ID:     uuid.NewString(),
		Kind:   kind,
		Detail: detail,
		Path:   path,
	}
	if st != nil {
		req.RunID = st.runID
		req.SessionID = st.session
	}
	decision, err := rt.decider.Decide(ctx, req)
	if err != nil {
		rt.recordDecision(ctx, kind, detail, path, false, err.Error())
		return agent.Wrap(agent.ErrPermissionDenied, kind, err)
	}
	if !decision.Allow {
		reason := decision.Reason
		if reason == "" {
			reason = "denied"
		}
		rt.recordDecision(ctx, kind, detail, path, false, reason)
		return agent.Wrap(agent.ErrPermissionDenied, kind, fmt.Errorf("%s", reason))
	}
	rt.recordDecision(ctx, kind, detail, path, true, decision.Reason)
	return nil
}

func (rt *Runtime) needsApproval(ctx context.Context) bool {
	if rt.cfg.Approvals.Required {
		return true
	}
	st := stateFrom(ctx)
	return st != nil && st.mode == agent.ModePlan && rt.cfg.PlanRequiresApproval()
}

func (rt *Runtime) recordDecision(ctx context.Context, kind, detail, path string, allow bool, reason string) {
	st := stateFrom(ctx)
	if st == nil {
		return
	}
	st.add(agent.Event{
		RunID:     st.runID,
		SessionID: st.session,
		Kind:      agent.EventPermissionRequest,
		Time:      time.Now().UTC(),
		Payload: agent.MustPayload(agent.PermissionPayload{
			ID:      uuid.NewString(),
			Kind:    kind,
			Detail:  detail,
			Path:    path,
			Allowed: allow,
			Reason:  reason,
		}),
	})
}

type gatedTool struct {
	inner tool.CallableTool
	kind  string
	rt    *Runtime
}

func (g *gatedTool) Declaration() *tool.Declaration { return g.inner.Declaration() }

func (g *gatedTool) Call(ctx context.Context, args []byte) (any, error) {
	path := ""
	var body map[string]any
	if json.Unmarshal(args, &body) == nil {
		if v, ok := body["file_name"].(string); ok {
			path = v
		}
	}
	if err := g.rt.authorize(ctx, g.kind, string(args), path); err != nil {
		return map[string]any{"denied": true, "error": err.Error()}, nil
	}
	return g.inner.Call(ctx, args)
}
