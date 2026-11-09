package trpcruntime

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
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
			if perm == agent.PermissionReadOnly {
				if asked != 0 {
					t.Fatalf("%s %s asked the approver %d times", perm, mode, asked)
				}
				if !strings.Contains(blob, "read_only") {
					t.Fatalf("%s %s missing read_only denial: %s", perm, mode, blob)
				}
				continue
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

type bareTool struct{}

func (bareTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "bare"}
}

func TestNonCallableToolIsRejected(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("bare", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("bare", `{}`), nil
	})
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Tools: []tool.Tool{bareTool{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "bare", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if !strings.Contains(blob, "not callable") {
		t.Fatalf("non-callable tool was not rejected: %s", blob)
	}
}

func TestApprovedHelperStillGatesChildTools(t *testing.T) {
	for _, tc := range []struct {
		perm string
		mode agent.Mode
	}{
		{agent.PermissionWorkspaceWrite, agent.ModeDefault},
		{agent.PermissionWorkspaceWrite, agent.ModePlan},
		{agent.PermissionReadOnly, agent.ModeDefault},
	} {
		var danger atomic.Int32
		dangerTool := function.NewFunctionTool(func(ctx context.Context, in struct {
			Text string `json:"text"`
		}) (string, error) {
			danger.Add(1)
			return "pwned", nil
		}, function.WithName("child_danger"), function.WithDescription("side effect"))
		cfg := config.Default()
		cfg.Workspace.Permission = tc.perm
		cfg.Approvals.Required = true
		cfg.Journal.Enabled = false
		cfg.Model.Generation.Stream = boolPtr(false)
		mdl := mockmodel.New("child-gate", func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if hasToolMessage(req) {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
			if strings.Contains(messagesText(req), "child-agent") {
				return toolCall("child_danger", `{"text":"x"}`), nil
			}
			return toolCall("helper", `{"request":"child-task"}`), nil
		})
		child := llmagent.New("helper",
			llmagent.WithModel(mdl),
			llmagent.WithInstruction("child-agent"),
			llmagent.WithEnableCodeExecutionResponseProcessor(false),
			llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
			llmagent.WithTools([]tool.Tool{dangerTool}),
		)
		rt, err := New(Options{
			Config: &cfg, Model: mdl, DataDir: t.TempDir(),
			Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
				if req.Detail == "helper" {
					return agent.Decision{ID: req.ID, Allow: true}, nil
				}
				return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
			}),
			SubAgents: []trpcagent.Agent{child},
		})
		if err != nil {
			t.Fatal(err)
		}
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: "cg-" + tc.perm + string(tc.mode), Message: "delegate", Mode: tc.mode,
		})
		if err != nil {
			rt.Close()
			t.Fatal(err)
		}
		blob := eventBlob(collect(t, h))
		rt.Close()
		if danger.Load() != 0 || strings.Contains(blob, "pwned") {
			t.Fatalf("%s %s ran child_danger %d: %s", tc.perm, tc.mode, danger.Load(), blob)
		}
		if tc.perm == agent.PermissionWorkspaceWrite && !strings.Contains(blob, "child_danger") {
			t.Fatalf("%s %s did not gate child_danger: %s", tc.perm, tc.mode, blob)
		}
	}
}

func TestTransferToAgentCannotRunSubagentTools(t *testing.T) {
	var danger atomic.Int32
	dangerTool := function.NewFunctionTool(func(ctx context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		danger.Add(1)
		return "pwned", nil
	}, function.WithName("child_danger"), function.WithDescription("side effect"))
	for _, perm := range []string{agent.PermissionReadOnly, agent.PermissionWorkspaceWrite} {
		danger.Store(0)
		cfg := config.Default()
		cfg.Workspace.Permission = perm
		cfg.Approvals.Required = true
		cfg.Journal.Enabled = false
		cfg.Model.Generation.Stream = boolPtr(false)
		mdl := mockmodel.New("xfer-"+perm, func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if strings.Contains(messagesText(req), "child-agent") {
				return toolCall("child_danger", `{"text":"x"}`), nil
			}
			if hasToolMessage(req) {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
			return toolCall("transfer_to_agent", `{"agent_name":"helper"}`), nil
		})
		child := llmagent.New("helper",
			llmagent.WithModel(mdl),
			llmagent.WithInstruction("child-agent"),
			llmagent.WithEnableCodeExecutionResponseProcessor(false),
			llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
			llmagent.WithTools([]tool.Tool{dangerTool}),
		)
		rt, err := New(Options{
			Config: &cfg, Model: mdl, DataDir: t.TempDir(),
			Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
				return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
			}),
			SubAgents: []trpcagent.Agent{child},
		})
		if err != nil {
			t.Fatal(err)
		}
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: "xfer-" + perm, Message: "delegate",
		})
		if err != nil {
			rt.Close()
			t.Fatal(err)
		}
		blob := eventBlob(collect(t, h))
		rt.Close()
		if danger.Load() != 0 {
			t.Fatalf("%s transfer ran child_danger %d times: %s", perm, danger.Load(), blob)
		}
		if strings.Contains(blob, "pwned") {
			t.Fatalf("%s leaked child tool result: %s", perm, blob)
		}
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
