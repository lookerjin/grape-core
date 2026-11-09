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

type readIn struct {
	FileName string `json:"file_name"`
}

type saveIn struct {
	FileName  string `json:"file_name"`
	Contents  string `json:"contents"`
	Overwrite bool   `json:"overwrite"`
}

type replaceIn struct {
	FileName        string `json:"file_name"`
	OldString       string `json:"old_string"`
	NewString       string `json:"new_string"`
	NumReplacements int    `json:"num_replacements"`
}

type searchIn struct {
	Path           string `json:"path"`
	Pattern        string `json:"pattern"`
	FilePattern    string `json:"file_pattern"`
	ContentPattern string `json:"content_pattern"`
}

type multiIn struct {
	Patterns []string `json:"patterns"`
}

func (rt *Runtime) workspaceFrom(ctx context.Context, fallback string) string {
	if v, ok := ctx.Value(workspaceKey{}).(string); ok && v != "" {
		return v
	}
	return fallback
}

func (rt *Runtime) fileTools(fallback string, allowWrite bool) []tool.Tool {
	ws := func(ctx context.Context) string { return rt.workspaceFrom(ctx, fallback) }
	tools := []tool.Tool{
		function.NewFunctionTool(func(ctx context.Context, in readIn) (map[string]any, error) {
			text, err := coding.Read(ws(ctx), in.FileName)
			if err != nil {
				return nil, err
			}
			return map[string]any{"file_name": in.FileName, "contents": text}, nil
		}, function.WithName("read_file"), function.WithDescription("Read a UTF-8 workspace file. Symlinks that leave the workspace are rejected.")),
		function.NewFunctionTool(func(ctx context.Context, in pathIn) (map[string]any, error) {
			names, err := coding.List(ws(ctx), in.Path)
			if err != nil {
				return nil, err
			}
			return map[string]any{"entries": names}, nil
		}, function.WithName("list_file"), function.WithDescription("List a workspace directory without following escaping symlinks.")),
		function.NewFunctionTool(func(ctx context.Context, in searchIn) (map[string]any, error) {
			pattern := in.Pattern
			if pattern == "" {
				pattern = in.FilePattern
			}
			files, dirs, err := coding.Match(ws(ctx), in.Path, pattern)
			if err != nil {
				return nil, err
			}
			return map[string]any{"files": files, "folders": dirs}, nil
		}, function.WithName("search_file"), function.WithDescription("Find workspace paths by name. Escaping symlinks are not followed.")),
		function.NewFunctionTool(func(ctx context.Context, in searchIn) (map[string]any, error) {
			hits, err := coding.Search(ws(ctx), in.ContentPattern)
			if err != nil {
				return nil, err
			}
			return map[string]any{"matches": hits}, nil
		}, function.WithName("search_content"), function.WithDescription("Search UTF-8 workspace files. Escaping symlinks are not followed.")),
		function.NewFunctionTool(func(ctx context.Context, in multiIn) (map[string]any, error) {
			var files []map[string]string
			for _, pattern := range in.Patterns {
				matches, _, err := coding.Match(ws(ctx), ".", pattern)
				if err != nil {
					return nil, err
				}
				for _, name := range matches {
					text, err := coding.Read(ws(ctx), name)
					if err != nil {
						files = append(files, map[string]string{"file_name": name, "error": err.Error()})
						continue
					}
					files = append(files, map[string]string{"file_name": name, "contents": text})
				}
			}
			return map[string]any{"files": files}, nil
		}, function.WithName("read_multiple_files"), function.WithDescription("Read workspace files matched by glob patterns.")),
	}
	if !allowWrite {
		return tools
	}
	tools = append(tools,
		function.NewFunctionTool(func(ctx context.Context, in saveIn) (map[string]any, error) {
			if err := rt.authorize(ctx, "write", in.FileName, in.FileName); err != nil {
				return map[string]any{"denied": true, "error": err.Error()}, nil
			}
			if err := coding.Save(ws(ctx), in.FileName, in.Contents, in.Overwrite); err != nil {
				return nil, err
			}
			return map[string]any{"file_name": in.FileName, "message": "saved"}, nil
		}, function.WithName("save_file"), function.WithDescription("Write a UTF-8 file inside the workspace. Symlinks that leave the workspace are rejected.")),
		function.NewFunctionTool(func(ctx context.Context, in replaceIn) (map[string]any, error) {
			if err := rt.authorize(ctx, "write", in.FileName, in.FileName); err != nil {
				return map[string]any{"denied": true, "error": err.Error()}, nil
			}
			if err := coding.Replace(ws(ctx), in.FileName, in.OldString, in.NewString, in.NumReplacements); err != nil {
				return nil, err
			}
			return map[string]any{"file_name": in.FileName, "message": "replaced"}, nil
		}, function.WithName("replace_content"), function.WithDescription("Replace text in a UTF-8 workspace file without following escaping symlinks.")),
	)
	return tools
}

func (rt *Runtime) workspaceTools(workspace string, allowWrite bool) ([]tool.Tool, error) {
	tools := rt.fileTools(workspace, allowWrite)
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
