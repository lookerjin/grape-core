package trpcruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

func TestExplicitZeroAllowsMoreThanTheDefaultPerAgent(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Context.MaxLLMCalls = config.Int(0)
	cfg.Context.MaxToolIterations = config.Int(0)
	cfg.Context.TotalMaxLLMCalls = config.Int(0)
	cfg.Context.TotalMaxToolIterations = config.Int(0)
	var steps atomic.Int32
	mdl := mockmodel.New("unlimited", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		text := messagesText(req)
		if strings.Contains(text, "child-agent") || strings.Contains(lastUserText(req), "child-loop") {
			if steps.Load() >= 130 {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("child-done")}}}, nil
			}
			return toolCall("child_step", `{"n":1}`), nil
		}
		if toolMessages(req) > 0 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("parent-done")}}}, nil
		}
		return toolCall("helper", `{"request":"child-loop"}`), nil
	})
	step := function.NewFunctionTool(func(ctx context.Context, in struct {
		N int `json:"n"`
	}) (string, error) {
		steps.Add(1)
		return "step", nil
	}, function.WithName("child_step"), function.WithDescription("one step"))
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Subagents: []SubagentSpec{{
			Name: "helper", Instruction: "child-agent", Model: mdl,
			Tools: []tool.Tool{step},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "zero-unlimited", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	if last.Kind != agent.EventCompleted {
		t.Fatalf("terminal %s %s", last.Kind, eventBlob(events))
	}
	if steps.Load() < 130 {
		t.Fatalf("child steps %d", steps.Load())
	}
}

func TestChildAgentLimitDoesNotFailTheRun(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Context.MaxLLMCalls = config.Int(2)
	cfg.Context.TotalMaxLLMCalls = config.Int(30)
	cfg.Context.MaxToolIterations = config.Int(30)
	cfg.Context.TotalMaxToolIterations = config.Int(30)
	mdl := mockmodel.New("child-limit", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		text := messagesText(req)
		if strings.Contains(text, "child-agent") || strings.Contains(lastUserText(req), "child-loop") {
			return toolCall("child_step", `{"n":1}`), nil
		}
		if toolMessages(req) > 0 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("parent-done")}}}, nil
		}
		return toolCall("helper", `{"request":"child-loop"}`), nil
	})
	step := function.NewFunctionTool(func(ctx context.Context, in struct {
		N int `json:"n"`
	}) (string, error) {
		return "step", nil
	}, function.WithName("child_step"), function.WithDescription("one step"))
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Subagents: []SubagentSpec{{
			Name: "helper", Instruction: "child-agent", Model: mdl,
			Tools: []tool.Tool{step},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "child-limit", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	if last.Kind != agent.EventCompleted {
		t.Fatalf("terminal %s %s", last.Kind, eventBlob(events))
	}
}

func TestRunTotalLLMCallsFailsTheRun(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Context.MaxLLMCalls = config.Int(50)
	cfg.Context.TotalMaxLLMCalls = config.Int(2)
	cfg.Context.MaxToolIterations = config.Int(50)
	cfg.Context.TotalMaxToolIterations = config.Int(50)
	mdl := mockmodel.New("run-total", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return toolCall("echo", `{"text":"again"}`), nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "run-total", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	body := mustTerminal(t, events[len(events)-1])
	if events[len(events)-1].Kind != agent.EventFailed || body.Reason != agent.ReasonBudgetExceeded || body.Scope != agent.ScopeRun {
		t.Fatalf("terminal %s %#v %s", events[len(events)-1].Kind, body, eventBlob(events))
	}
}

func TestDeniedAttemptsCountTowardMaxToolCalls(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = true
	cfg.Context.MaxToolCalls = config.Int(1)
	cfg.Context.MaxLLMCalls = config.Int(8)
	cfg.Context.TotalMaxLLMCalls = config.Int(8)
	mdl := mockmodel.New("deny-budget", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if toolMessages(req) >= 2 || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("echo", `{"text":"again"}`), nil
	})
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Tools: []tool.Tool{echoTool()},
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			return agent.Decision{Allow: false, Reason: "no"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "deny-budget", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	body := mustTerminal(t, events[len(events)-1])
	if events[len(events)-1].Kind != agent.EventFailed || body.Reason != agent.ReasonBudgetExceeded || body.Scope != agent.ScopeRun {
		t.Fatalf("terminal %s %#v %s", events[len(events)-1].Kind, body, eventBlob(events))
	}
}

func TestMaxToolCallsEndsTheRunBeforeTheNextModelCall(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Context.MaxToolCalls = config.Int(2)
	cfg.Context.MaxLLMCalls = config.Int(0)
	cfg.Context.TotalMaxLLMCalls = config.Int(0)
	cfg.Context.MaxToolIterations = config.Int(0)
	cfg.Context.TotalMaxToolIterations = config.Int(0)
	var calls atomic.Int32
	var ran atomic.Int32
	mdl := mockmodel.New("tool-cap", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		calls.Add(1)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return toolCall("echo", `{"text":"again"}`), nil
	})
	echo := function.NewFunctionTool(func(ctx context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		ran.Add(1)
		return in.Text, nil
	}, function.WithName("echo"), function.WithDescription("echo"))
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{echo}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "tool-cap", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	body := mustTerminal(t, last)
	if last.Kind != agent.EventFailed || body.Reason != agent.ReasonBudgetExceeded || body.Scope != agent.ScopeRun {
		t.Fatalf("terminal %s %#v calls=%d ran=%d %s", last.Kind, body, calls.Load(), ran.Load(), eventBlob(events))
	}
	if calls.Load() != 3 || ran.Load() != 2 {
		t.Fatalf("model calls %d tool runs %d", calls.Load(), ran.Load())
	}
}

func TestToolAttemptReserveIsAtomic(t *testing.T) {
	st := newRunState("r", "s", agent.ModeDefault)
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if st.reserveToolCall(1) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 1 {
		t.Fatalf("allowed %d", allowed.Load())
	}
	st.mu.Lock()
	got := st.toolCalls
	st.mu.Unlock()
	if got != 32 {
		t.Fatalf("attempts %d", got)
	}
}

func TestStopReasonUsesTypeAndCountNotMessageText(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Context.MaxLLMCalls = config.Int(5)
	rt := &Runtime{cfg: cfg}
	st := newRunState("r", "s", agent.ModeDefault)
	st.noteRoot("inv-root")
	st.addLLM("inv-root")

	rt.observeLimit(st, stopEvent("inv-child", "helper", "max LLM calls (5) exceeded"))
	if _, _, _, ok := st.stop(); ok {
		t.Fatal("child stop changed the run")
	}

	rt.observeLimit(st, stopEvent("inv-root", "grape", "max LLM calls (5) exceeded"))
	reason, scope, agentID, ok := st.stop()
	if !ok || reason != agent.ReasonAgentStopped || scope != agent.ScopeAgent || agentID != "grape" {
		t.Fatalf("unconfirmed stop %q %q %q", reason, scope, agentID)
	}

	confirmed := newRunState("r", "s", agent.ModeDefault)
	confirmed.noteRoot("inv-root")
	for i := 0; i < 5; i++ {
		confirmed.addLLM("inv-root")
	}
	rt.observeLimit(confirmed, stopEvent("inv-root", "grape", "stopped for another reason"))
	reason, scope, agentID, ok = confirmed.stop()
	if !ok || reason != agent.ReasonBudgetExceeded || scope != agent.ScopeAgent || agentID != "grape" {
		t.Fatalf("confirmed stop %q %q %q", reason, scope, agentID)
	}
}

func TestFlowErrorTextIsNotABudget(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	mdl := mockmodel.New("plain-fail", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		return nil, errors.New("max LLM calls (3) exceeded")
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "plain-fail", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	body := mustTerminal(t, last)
	if last.Kind != agent.EventFailed {
		t.Fatalf("terminal %s %#v", last.Kind, body)
	}
	if body.Reason == agent.ReasonBudgetExceeded || body.Scope == agent.ScopeRun || body.Scope == agent.ScopeAgent {
		t.Fatalf("text was classified as a budget: %#v", body)
	}
}

func stopEvent(invocation, author, message string) *event.Event {
	return &event.Event{
		InvocationID: invocation,
		Author:       author,
		Response: &model.Response{
			Done: true,
			Error: &model.ResponseError{
				Type:    trpcagent.ErrorTypeStopAgentError,
				Message: message,
			},
		},
	}
}

func mustTerminal(t *testing.T, evt agent.Event) agent.TerminalPayload {
	t.Helper()
	var body agent.TerminalPayload
	if err := json.Unmarshal(evt.Payload, &body); err != nil {
		t.Fatal(err)
	}
	return body
}
