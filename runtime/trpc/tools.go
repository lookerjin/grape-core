package trpcruntime

import (
	"context"
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
	mu       sync.Mutex
	cond     *sync.Cond
	events   []agent.Event
	runID    string
	session  string
	mode     agent.Mode
	tools    int
	stopping bool
}

func newRunState(runID, session string, mode agent.Mode) *runState {
	st := &runState{runID: runID, session: session, mode: mode}
	st.cond = sync.NewCond(&st.mu)
	return st
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

func (st *runState) enterTool() {
	if st == nil {
		return
	}
	st.mu.Lock()
	st.tools++
	st.mu.Unlock()
}

func (st *runState) leaveTool() {
	if st == nil {
		return
	}
	st.mu.Lock()
	if st.tools > 0 {
		st.tools--
	}
	if st.tools == 0 {
		st.cond.Broadcast()
	}
	st.mu.Unlock()
}

// waitTools blocks until every tool that has entered has left.
// The count and the wait share one mutex, so a tool cannot publish
// itself after a waiter has already decided the run is idle.
func (st *runState) waitTools() {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stopping = true
	for st.tools > 0 {
		st.cond.Wait()
	}
}

// captureBusy marks the run as finishing and reports whether a tool is
// still inside Call. The flag and the counter move together, so a tool
// that enters before this returns is included.
func (st *runState) captureBusy() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stopping = true
	return st.tools > 0
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
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
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
			text, err := coding.ReadLimited(ws(ctx), in.FileName, coding.ReadLimit{
				MaxBytes:   rt.cfg.Workspace.MaxReadBytes,
				LineOffset: in.Offset,
				LineLimit:  in.Limit,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"file_name": in.FileName, "contents": text}, nil
		}, function.WithName("read_file"), function.WithDescription("Read a UTF-8 workspace file, with optional 1-based offset and line limit. Symlinks that leave the workspace are rejected.")),
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
			path := in.Path
			if path == "" {
				path = "."
			}
			hits, err := coding.SearchLimited(ws(ctx), path, in.ContentPattern, coding.SearchLimit{
				MaxBytes: rt.cfg.Workspace.MaxReadBytes,
				MaxHits:  rt.cfg.Workspace.MaxSearchHits,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"matches": hits}, nil
		}, function.WithName("search_content"), function.WithDescription("Search UTF-8 files under path. Escaping symlinks are not followed. Large files and excess matches are skipped.")),
		function.NewFunctionTool(func(ctx context.Context, in multiIn) (map[string]any, error) {
			var files []map[string]string
			for _, pattern := range in.Patterns {
				matches, _, err := coding.Match(ws(ctx), ".", pattern)
				if err != nil {
					return nil, err
				}
				for _, name := range matches {
					text, err := coding.ReadLimited(ws(ctx), name, coding.ReadLimit{MaxBytes: rt.cfg.Workspace.MaxReadBytes})
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

// ToolSpec classifies a tool supplied by the caller.
type ToolSpec struct {
	Tool tool.Tool
	// Kind is read, write, command, or side_effect. Empty means side_effect.
	Kind string
	// Trusted skips the approval and read_only gate.
	Trusted bool
}

type gatedTool struct {
	rt    *Runtime
	inner tool.CallableTool
	kind  string
}

func (t gatedTool) Declaration() *tool.Declaration { return t.inner.Declaration() }

func (t gatedTool) Call(ctx context.Context, args []byte) (any, error) {
	name := ""
	if decl := t.Declaration(); decl != nil {
		name = decl.Name
	}
	if err := t.rt.authorize(ctx, t.kind, name, ""); err != nil {
		return map[string]any{"denied": true, "error": err.Error()}, nil
	}
	return t.inner.Call(ctx, args)
}

func (rt *Runtime) gate(item tool.Tool, kind string) tool.Tool {
	callable, ok := item.(tool.CallableTool)
	if !ok || rt == nil {
		return rejectedTool{inner: item, rt: rt, kind: kind}
	}
	return gatedTool{rt: rt, inner: callable, kind: kind}
}

// rejectedTool is a non-callable tool exposed as a callable denial so it
// cannot skip the approval gate.
type rejectedTool struct {
	inner tool.Tool
	rt    *Runtime
	kind  string
}

func (t rejectedTool) Declaration() *tool.Declaration {
	if t.inner != nil && t.inner.Declaration() != nil {
		return t.inner.Declaration()
	}
	return &tool.Declaration{Name: "rejected"}
}

func (t rejectedTool) Call(ctx context.Context, args []byte) (any, error) {
	name := t.Declaration().Name
	if t.rt != nil {
		if err := t.rt.authorize(ctx, t.kind, name, ""); err != nil {
			return map[string]any{"denied": true, "error": err.Error()}, nil
		}
	}
	return map[string]any{"denied": true, "error": "tool is not callable"}, nil
}

type trackingTool struct{ inner tool.CallableTool }

func (t trackingTool) Declaration() *tool.Declaration { return t.inner.Declaration() }

func (t trackingTool) Call(ctx context.Context, args []byte) (any, error) {
	st := stateFrom(ctx)
	if st != nil {
		st.enterTool()
		defer st.leaveTool()
	}
	return t.inner.Call(ctx, args)
}

func trackTool(item tool.Tool) tool.Tool {
	callable, ok := item.(tool.CallableTool)
	if !ok {
		return item
	}
	return trackingTool{inner: callable}
}

func trackTools(tools []tool.Tool) []tool.Tool {
	out := make([]tool.Tool, len(tools))
	for i, item := range tools {
		out[i] = trackTool(item)
	}
	return out
}

func mutatingKind(kind string) bool {
	switch kind {
	case "command", "write", "side_effect":
		return true
	default:
		return false
	}
}

func (rt *Runtime) authorize(ctx context.Context, kind, detail, path string) error {
	if rt.cfg.Workspace.Permission == agent.PermissionReadOnly && mutatingKind(kind) {
		rt.recordDecision(ctx, kind, detail, path, false, "read_only")
		return agent.Wrap(agent.ErrPermissionDenied, kind, fmt.Errorf("read_only workspace"))
	}
	if mutatingKind(kind) && rt.needsApproval(ctx) {
		if err := rt.decide(ctx, kind, detail, path); err != nil {
			return err
		}
	}
	return nil
}

func (rt *Runtime) decide(ctx context.Context, kind, detail, path string) error {
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
