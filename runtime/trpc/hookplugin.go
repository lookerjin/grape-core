package trpcruntime

import (
	"context"
	"encoding/json"

	"github.com/lookerjin/grape-core/hooks"
	trpcplugin "trpc.group/trpc-go/trpc-agent-go/plugin"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type hookPlugin struct {
	rt *Runtime
}

func (p *hookPlugin) Name() string { return "grape-hooks" }

func (p *hookPlugin) Register(reg *trpcplugin.Registry) {
	if reg == nil {
		return
	}
	reg.BeforeTool(p.beforeTool)
}

func (p *hookPlugin) beforeTool(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
	if p == nil || p.rt == nil || p.rt.hookRunner == nil || args == nil {
		return nil, nil
	}
	workspace := p.rt.cfg.Workspace.Root
	if v, ok := ctx.Value(workspaceKey{}).(string); ok && v != "" {
		workspace = v
	}
	payload, _ := json.Marshal(map[string]any{
		"name": args.ToolName,
		"id":   args.ToolCallID,
	})
	_, err := p.rt.hookRunner.Run(ctx, hooks.BeforeTool, workspace, string(payload))
	if err != nil {
		return &tool.BeforeToolResult{CustomResult: map[string]any{
			"denied": true,
			"error":  err.Error(),
		}}, nil
	}
	return nil, nil
}
