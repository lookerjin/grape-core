package trpcruntime

import (
	"context"
	"fmt"
	"slices"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/sandbox"
	native "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func effectivePermission(ctx context.Context, fallback string) string {
	if st := stateFrom(ctx); st != nil && st.permission != "" {
		return st.permission
	}
	return fallback
}

func (rt *Runtime) runAllowsWrite(ro native.RunOptions) bool {
	p, _ := ro.RuntimeState["permission"].(string)
	if p == "" {
		p = rt.cfg.Workspace.Permission
	}
	return p != agent.PermissionReadOnly
}

func (rt *Runtime) runWorkspaceTools(ws string, ro native.RunOptions) []tool.Tool {
	tools := rt.workspaceTools(ws, rt.runAllowsWrite(ro))
	if ro.RuntimeState["permission"] == agent.PermissionReadOnly {
		tools = slices.DeleteFunc(tools, func(t tool.Tool) bool { return t.Declaration().Name == "run_command" })
	}
	return tools
}

func opaqueTools(children []SubagentSpec, tools []ToolSpec) bool {
	for _, t := range tools {
		if t.Trusted {
			return true
		}
	}
	for _, s := range children {
		if len(s.ToolSets) > 0 || opaqueTools(s.Subagents, s.ToolSpecs) {
			return true
		}
	}
	return false
}

func (rt *Runtime) checkProfile(req agent.RunRequest) error {
	ceiling := rt.cfg.Workspace.Permission
	switch req.Permission {
	case "", agent.PermissionReadOnly:
	case agent.PermissionWorkspaceWrite:
		if ceiling == agent.PermissionReadOnly || rt.cfg.Sandbox.Mode == sandbox.ModeDangerFullAccess {
			return agent.Invalid("permission", fmt.Errorf("workspace write requires a compatible runtime ceiling"))
		}
	case agent.PermissionDangerFullAccess:
		if ceiling != req.Permission {
			return agent.Invalid("permission", fmt.Errorf("exceeds runtime ceiling"))
		}
	default:
		return agent.Invalid("permission", fmt.Errorf("unknown profile %q", req.Permission))
	}
	if (req.Permission != "" || req.RequireApproval) && (rt.profileUnsafe || rt.hookRunner != nil) {
		return agent.Invalid("permission", fmt.Errorf("per-turn restriction incompatible with trusted extensions, hooks or executable skills"))
	}
	return nil
}
