package trpcruntime

import (
	"context"
	"fmt"

	"github.com/lookerjin/grape-core/agent"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
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
		case kindedTool:
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

func rejectSpecNames(specs []SubagentSpec) error {
	for _, spec := range specs {
		if spec.Name == rootAgentName {
			return agent.Wrap(agent.ErrInvalidConfig, "agent", fmt.Errorf("non-root agent named %q is reserved", rootAgentName))
		}
		if err := rejectSpecNames(spec.Subagents); err != nil {
			return err
		}
	}
	return nil
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
