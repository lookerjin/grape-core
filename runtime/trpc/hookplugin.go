package trpcruntime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/hooks"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
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
	reg.BeforeAgent(p.beforeAgent)
	reg.AfterAgent(p.afterAgent)
	reg.BeforeModel(p.beforeModel)
	reg.AfterModel(p.afterModel)
	reg.BeforeTool(p.beforeTool)
	reg.AfterTool(p.afterTool)
	reg.OnEvent(p.onEvent)
	reg.AfterRun(p.afterRun)
}

func (p *hookPlugin) beforeAgent(ctx context.Context, args *trpcagent.BeforeAgentArgs) (*trpcagent.BeforeAgentResult, error) {
	if args != nil && args.Invocation != nil {
		if st := stateFrom(ctx); st != nil {
			st.noteRoot(args.Invocation.InvocationID)
		}
	}
	if err := p.dispatch(ctx, hooks.BeforeRun, `{"event":"BeforeRun"}`); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *hookPlugin) afterAgent(ctx context.Context, args *trpcagent.AfterAgentArgs) (*trpcagent.AfterAgentResult, error) {
	// AfterRun is emitted from the runner hook so a run does not fire it twice.
	return nil, nil
}

func (p *hookPlugin) beforeModel(ctx context.Context, args *model.BeforeModelArgs) (*model.BeforeModelResult, error) {
	if p.rt != nil {
		if err := p.rt.consumeLLMCall(ctx); err != nil {
			return nil, err
		}
	}
	if err := p.dispatch(ctx, hooks.BeforeModel, `{"event":"BeforeModel"}`); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *hookPlugin) afterModel(ctx context.Context, args *model.AfterModelArgs) (*model.AfterModelResult, error) {
	if args != nil && args.Response != nil && args.Response.IsPartial {
		return nil, nil
	}
	if p.rt != nil {
		resp := (*model.Response)(nil)
		if args != nil {
			resp = args.Response
		}
		if err := p.rt.noteToolRound(ctx, resp); err != nil {
			return nil, err
		}
	}
	if err := p.dispatch(ctx, hooks.AfterModel, `{"event":"AfterModel"}`); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *hookPlugin) beforeTool(ctx context.Context, args *tool.BeforeToolArgs) (*tool.BeforeToolResult, error) {
	if args == nil {
		return nil, nil
	}
	payload, _ := json.Marshal(map[string]any{
		"name":      args.ToolName,
		"id":        args.ToolCallID,
		"arguments": json.RawMessage(args.Arguments),
	})
	if err := p.dispatch(ctx, hooks.BeforeTool, string(payload)); err != nil {
		return &tool.BeforeToolResult{CustomResult: map[string]any{
			"denied": true,
			"error":  err.Error(),
		}}, nil
	}
	if p.rt != nil {
		if err := p.rt.consumeToolCall(ctx); err != nil {
			return nil, err
		}
		if p.rt.nestedCaller(ctx) {
			kind := p.rt.nestedToolKind(ctx, args.ToolName)
			if err := p.rt.authorize(ctx, kind, args.ToolName, ""); err != nil {
				return &tool.BeforeToolResult{CustomResult: map[string]any{
					"denied": true,
					"error":  err.Error(),
				}}, nil
			}
		}
	}
	return nil, nil
}

func (p *hookPlugin) afterTool(ctx context.Context, args *tool.AfterToolArgs) (*tool.AfterToolResult, error) {
	name := ""
	if args != nil {
		name = args.ToolName
	}
	payload, _ := json.Marshal(map[string]any{"name": name, "event": "AfterTool"})
	if err := p.dispatch(ctx, hooks.AfterTool, string(payload)); err != nil {
		return nil, err
	}
	return nil, nil
}

func (p *hookPlugin) onEvent(ctx context.Context, inv *trpcagent.Invocation, evt *event.Event) (*event.Event, error) {
	if evt != nil && evt.Response != nil && evt.IsPartial {
		return evt, nil
	}
	kind := ""
	if evt != nil {
		kind = string(evt.Object)
	}
	payload, _ := json.Marshal(map[string]any{"event": "OnEvent", "object": kind})
	if err := p.dispatch(ctx, hooks.OnEvent, string(payload)); err != nil {
		return evt, err
	}
	return evt, nil
}

func (p *hookPlugin) afterRun(ctx context.Context, args *trpcplugin.AfterRunArgs) error {
	return p.dispatch(ctx, hooks.AfterRun, `{"event":"AfterRun","where":"runner"}`)
}

func (p *hookPlugin) dispatch(ctx context.Context, eventName, payload string) error {
	if p == nil || p.rt == nil || p.rt.hookRunner == nil {
		return nil
	}
	results, err := p.rt.hookRunner.Run(ctx, eventName, p.workspace(ctx), payload)
	if st := stateFrom(ctx); st != nil {
		for _, res := range results {
			if res.Err == "" || res.Blocked {
				continue
			}
			st.add(agent.Event{
				Kind: agent.EventError,
				Time: time.Now().UTC(),
				Payload: agent.MustPayload(map[string]string{
					"hook":  res.Event,
					"error": res.Err,
				}),
			})
		}
	}
	return err
}

func (p *hookPlugin) workspace(ctx context.Context) string {
	if p == nil || p.rt == nil {
		return ""
	}
	workspace := p.rt.cfg.Workspace.Root
	if v, ok := ctx.Value(workspaceKey{}).(string); ok && v != "" {
		workspace = v
	}
	return workspace
}
