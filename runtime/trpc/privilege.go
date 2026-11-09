package trpcruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/lookerjin/grape-core/agent"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
)

const rootAgentName = "grape"

// NestedRead is an operator allowlist entry. A nested caller is treated as
// read only when its agent name and the tool name both match. The tool's
// own description is not consulted.
type NestedRead struct {
	Agent string
	Tool  string
}

// grapeReadLevel is implemented only by grape's own tool types. The method
// is unexported, so another module cannot satisfy the check.
type grapeReadLevel interface {
	grapeReadLevel() bool
}

// builtinReadTool marks a grape-built read tool. Identity is the concrete
// type, not the tool's name or description.
type builtinReadTool struct {
	inner tool.CallableTool
}

func (t builtinReadTool) grapeReadLevel() bool { return true }

func (t builtinReadTool) Declaration() *tool.Declaration {
	if t.inner == nil {
		return nil
	}
	return t.inner.Declaration()
}

func (t builtinReadTool) Call(ctx context.Context, args []byte) (any, error) {
	return t.inner.Call(ctx, args)
}

func (rt *Runtime) asRead(item tool.Tool) tool.Tool {
	callable, ok := item.(tool.CallableTool)
	if !ok {
		return item
	}
	return &builtinReadTool{inner: callable}
}

// grapeReadTool wraps a read-level tool grape constructed, such as a
// knowledge skill tool. The type is unexported, so another module cannot
// build a value that passes the nested read check.
type grapeReadTool struct {
	inner tool.CallableTool
}

func (t *grapeReadTool) Declaration() *tool.Declaration {
	if t == nil || t.inner == nil {
		return nil
	}
	return t.inner.Declaration()
}

func (t *grapeReadTool) Call(ctx context.Context, args []byte) (any, error) {
	return t.inner.Call(ctx, args)
}

func wrapGrapeRead(item tool.Tool) tool.Tool {
	callable, ok := item.(tool.CallableTool)
	if !ok || item == nil {
		return item
	}
	return &grapeReadTool{inner: callable}
}

func (t hostMCPTool) grapeReadLevel() bool {
	return t.kind == "read"
}

func nestedReadIndex(entries []NestedRead) map[string]map[string]struct{} {
	if len(entries) == 0 {
		return nil
	}
	out := map[string]map[string]struct{}{}
	for _, entry := range entries {
		if entry.Agent == "" || entry.Tool == "" {
			continue
		}
		if out[entry.Agent] == nil {
			out[entry.Agent] = map[string]struct{}{}
		}
		out[entry.Agent][entry.Tool] = struct{}{}
	}
	return out
}

func (rt *Runtime) allowNestedRead(agentName, toolName string) bool {
	if rt == nil || rt.nested == nil {
		return false
	}
	tools := rt.nested[agentName]
	if tools == nil {
		return false
	}
	_, ok := tools[toolName]
	return ok
}

// nestedCaller reports whether this tool call is outside the root invocation
// captured at run start. The agent name is not the signal: a nested agent
// named "grape" is still nested when its invocation ID differs.
func (rt *Runtime) nestedCaller(ctx context.Context) bool {
	inv, ok := trpcagent.InvocationFromContext(ctx)
	if !ok || inv == nil || inv.InvocationID == "" {
		return true
	}
	st := stateFrom(ctx)
	if st == nil {
		return true
	}
	root := st.rootInvocation()
	if root == "" {
		return true
	}
	return inv.InvocationID != root
}

// nestedToolKind classifies a nested tool call. Read is granted only by
// grape's own tool types, a user-config MCP server level carried on
// hostMCPTool, or Options.NestedReads. Names and description text grant nothing.
func (rt *Runtime) nestedToolKind(ctx context.Context, name string) string {
	inv, _ := trpcagent.InvocationFromContext(ctx)
	agentName := ""
	if inv != nil {
		agentName = inv.AgentName
	}
	if rt.allowNestedRead(agentName, name) {
		return "read"
	}
	matched := toolsNamed(inv, name)
	if len(matched) == 0 {
		return "side_effect"
	}
	for _, item := range matched {
		if !isGrapeRead(item) {
			return "side_effect"
		}
	}
	return "read"
}

// isGrapeRead reports whether the tool is one grape constructed. The check
// is a type assertion on unexported types. It does not use pointer identity.
func isGrapeRead(item tool.Tool) bool {
	switch t := unwrapGrape(item).(type) {
	case *builtinReadTool:
		return t != nil
	case *grapeReadTool:
		return t != nil
	case *hostMCPTool:
		return t != nil && t.kind == "read"
	default:
		return false
	}
}

func toolsNamed(inv *trpcagent.Invocation, name string) []tool.Tool {
	if inv == nil || inv.Agent == nil || name == "" {
		return nil
	}
	var out []tool.Tool
	for _, item := range inv.Agent.Tools() {
		if item == nil || item.Declaration() == nil {
			continue
		}
		if item.Declaration().Name == name {
			out = append(out, item)
		}
	}
	return out
}

// unwrapGrape removes only wrappers this package constructs. It does not
// follow Original or any other exported unwrap method.
func unwrapGrape(item tool.Tool) tool.Tool {
	for i := 0; item != nil && i < 8; i++ {
		switch inner := item.(type) {
		case trackingTool:
			if inner.inner == nil {
				return item
			}
			item = inner.inner
		case gatedTool:
			if inner.inner == nil {
				return item
			}
			item = inner.inner
		default:
			return item
		}
	}
	return item
}

func (rt *Runtime) consumeLLMCall(ctx context.Context) error {
	st := stateFrom(ctx)
	if rt == nil || st == nil {
		return nil
	}
	limit := rt.cfg.Context.MaxLLMCalls
	n := st.addLLMCall()
	if limit > 0 && n > limit {
		return rt.noteBudget(ctx, fmt.Errorf("max LLM calls (%d) exceeded", limit))
	}
	return nil
}

func (rt *Runtime) consumeToolCall(ctx context.Context) error {
	st := stateFrom(ctx)
	if rt == nil || st == nil {
		return nil
	}
	limit := rt.cfg.Context.MaxToolCalls
	if limit <= 0 {
		return nil
	}
	n := st.addToolCall()
	if n > limit {
		return rt.noteBudget(ctx, fmt.Errorf("max tool calls (%d) exceeded", limit))
	}
	return nil
}

func (rt *Runtime) noteToolRound(ctx context.Context, resp *model.Response) error {
	if rt == nil || !responseRequestsTools(resp) {
		return nil
	}
	st := stateFrom(ctx)
	if st == nil {
		return nil
	}
	limit := rt.cfg.Context.MaxToolIterations
	n := st.addToolRound()
	if limit > 0 && n > limit {
		return rt.noteBudget(ctx, fmt.Errorf("max tool iterations (%d) exceeded", limit))
	}
	return nil
}

func responseRequestsTools(resp *model.Response) bool {
	if resp == nil || resp.IsPartial {
		return false
	}
	for _, choice := range resp.Choices {
		if len(choice.Message.ToolCalls) > 0 || len(choice.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

func (rt *Runtime) noteBudget(ctx context.Context, err error) error {
	if st := stateFrom(ctx); st != nil && err != nil {
		st.markFailure(err.Error())
		st.add(agent.Event{
			Kind: agent.EventError,
			Time: time.Now().UTC(),
			Payload: agent.MustPayload(agent.ErrorPayload{
				Message: err.Error(),
				Type:    "budget",
			}),
		})
	}
	return err
}

func rejectReservedAgents(root trpcagent.Agent, subs []trpcagent.Agent, tools []tool.Tool, extra []tool.Tool) error {
	seen := map[trpcagent.Agent]struct{}{}
	var walk func(trpcagent.Agent, bool) error
	walk = func(a trpcagent.Agent, isRoot bool) error {
		if a == nil {
			return nil
		}
		if _, ok := seen[a]; ok {
			return nil
		}
		seen[a] = struct{}{}
		if !isRoot && a.Info().Name == rootAgentName {
			return agent.Wrap(agent.ErrInvalidConfig, "agent", fmt.Errorf("non-root agent named %q is reserved", rootAgentName))
		}
		if err := rejectReservedTools(a.Tools()); err != nil {
			return err
		}
		for _, sub := range a.SubAgents() {
			if err := walk(sub, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, true); err != nil {
		return err
	}
	for _, sub := range subs {
		if err := walk(sub, false); err != nil {
			return err
		}
	}
	if err := rejectReservedTools(tools); err != nil {
		return err
	}
	return rejectReservedTools(extra)
}

func specTools(specs []ToolSpec) []tool.Tool {
	out := make([]tool.Tool, 0, len(specs))
	for _, spec := range specs {
		if spec.Tool != nil {
			out = append(out, spec.Tool)
		}
	}
	return out
}

func rejectReservedTools(tools []tool.Tool) error {
	for _, item := range tools {
		if _, ok := item.(*agenttool.Tool); !ok || item == nil || item.Declaration() == nil {
			continue
		}
		if item.Declaration().Name == rootAgentName {
			return agent.Wrap(agent.ErrInvalidConfig, "agent", fmt.Errorf("non-root agent named %q is reserved", rootAgentName))
		}
	}
	return nil
}
