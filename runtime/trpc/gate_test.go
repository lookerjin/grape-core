package trpcruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestUntrustedToolsAndSubagentsAreGated(t *testing.T) {
	deny := agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
		return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
	})
	for _, perm := range []string{agent.PermissionReadOnly, agent.PermissionWorkspaceWrite} {
		for _, mode := range []agent.Mode{agent.ModeDefault, agent.ModePlan} {
			cfg := config.Default()
			cfg.Workspace.Permission = perm
			cfg.Approvals.Required = true
			cfg.Journal.Enabled = false
			cfg.Model.Generation.Stream = boolPtr(false)
			var asked int
			mdl := mockmodel.New("gate", func(ctx context.Context, req *model.Request) (*model.Response, error) {
				if hasToolMessage(req) {
					return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
				}
				return toolCall("echo", `{"text":"x"}`), nil
			})
			decider := agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
				asked++
				return deny.Decide(ctx, req)
			})
			rt, err := New(Options{
				Config:  &cfg,
				Model:   mdl,
				DataDir: t.TempDir(),
				Decider: decider,
				Tools:   []tool.Tool{echoTool()},
			})
			if err != nil {
				t.Fatal(err)
			}
			h, err := rt.Start(context.Background(), agent.RunRequest{
				UserID: "u", SessionID: "g-" + perm + "-" + string(mode), Message: "go", Mode: mode,
			})
			if err != nil {
				rt.Close()
				t.Fatal(err)
			}
			blob := eventBlob(collect(t, h))
			rt.Close()
			if !strings.Contains(blob, "denied") {
				t.Fatalf("%s %s ran the extra tool: %s", perm, mode, blob)
			}
			if asked == 0 {
				t.Fatalf("%s %s asked=0: %s", perm, mode, blob)
			}
		}
	}

	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = true
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	var childCalls int
	mdl := mockmodel.New("sub", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if strings.Contains(lastUserText(req), "child-task") {
			childCalls++
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("child-ok")}}}, nil
		}
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("parent-done")}}}, nil
		}
		return toolCall("helper", `{"request":"child-task"}`), nil
	})
	child := llmagent.New("helper",
		llmagent.WithModel(mdl),
		llmagent.WithInstruction("answer"),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
	)
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(), Decider: deny,
		SubAgents: []trpcagent.Agent{child},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "subgate", Message: "delegate"})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if childCalls != 0 || strings.Contains(blob, "child-ok") {
		t.Fatalf("subagent ran without approval calls=%d %s", childCalls, blob)
	}
	if !strings.Contains(blob, "denied") {
		t.Fatalf("subagent was not gated: %s", blob)
	}
}

func toolCall(name, args string) *model.Response {
	return &model.Response{Choices: []model.Choice{{
		Message: model.Message{
			Role: model.RoleAssistant,
			ToolCalls: []model.ToolCall{{
				ID:   "call-" + name,
				Type: "function",
				Function: model.FunctionDefinitionParam{
					Name:      name,
					Arguments: []byte(args),
				},
			}},
		},
	}}}
}
