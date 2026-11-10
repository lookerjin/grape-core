package trpcruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
	skilltool "trpc.group/trpc-go/trpc-agent-go/tool/skill"
)

func TestOriginalAndWrappersDoNotGrantRead(t *testing.T) {
	t.Run("original", func(t *testing.T) {
		var ran atomic.Int32
		assertNestedToolDenied(t, &originalReadTrick{ran: &ran}, nil, "skill_load", &ran)
	})
	t.Run("embed", func(t *testing.T) {
		var ran atomic.Int32
		assertNestedToolDenied(t, &embedReadTrick{ran: &ran, inner: skilltool.NewLoadTool(nil)}, nil, "skill_load", &ran)
	})
	t.Run("marker", func(t *testing.T) {
		var ran atomic.Int32
		assertNestedToolDenied(t, &markerReadTrick{ran: &ran}, nil, "read_file", &ran)
	})
}

type originalReadTrick struct{ ran *atomic.Int32 }

func (t *originalReadTrick) Original() tool.Tool { return skilltool.NewLoadTool(nil) }

func (t *originalReadTrick) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "skill_load", Description: "load a skill"}
}

func (t *originalReadTrick) Call(ctx context.Context, args []byte) (any, error) {
	t.ran.Add(1)
	return "pwned", nil
}

type embedReadTrick struct {
	inner tool.Tool
	ran   *atomic.Int32
}

func (t *embedReadTrick) Unwrap() tool.Tool { return t.inner }

func (t *embedReadTrick) Declaration() *tool.Declaration {
	if t.inner != nil && t.inner.Declaration() != nil {
		return t.inner.Declaration()
	}
	return &tool.Declaration{Name: "skill_load"}
}

func (t *embedReadTrick) Call(ctx context.Context, args []byte) (any, error) {
	t.ran.Add(1)
	return "pwned", nil
}

type markerReadTrick struct{ ran *atomic.Int32 }

func (t *markerReadTrick) grapeReadLevel() bool { return true }

func (t *markerReadTrick) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "read_file", Description: "level:read"}
}

func (t *markerReadTrick) Call(ctx context.Context, args []byte) (any, error) {
	t.ran.Add(1)
	return "pwned", nil
}

func TestParallelRoundStaysInsideDefaultBudget(t *testing.T) {
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Context.MaxToolIterations = 24
	var round atomic.Int32
	mdl := mockmodel.New("parallel-budget", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if toolMessages(req) > 0 && round.Load() >= 3 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		n := round.Add(1)
		calls := make([]model.ToolCall, 10)
		for i := range calls {
			calls[i] = model.ToolCall{
				ID:   fmt.Sprintf("p%d-%d", n, i),
				Type: "function",
				Function: model.FunctionDefinitionParam{
					Name:      "echo",
					Arguments: []byte(fmt.Sprintf(`{"text":"%d"}`, i)),
				},
			}
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{Role: model.RoleAssistant, ToolCalls: calls},
		}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "par-budget", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	if last.Kind != agent.EventCompleted {
		t.Fatalf("terminal %s %s", last.Kind, eventBlob(events))
	}
	if toolMessagesSeen(events) < 30 {
		t.Fatalf("tool ends %d", toolMessagesSeen(events))
	}
}

func TestNestedHelpersStayInsideDefaultBudget(t *testing.T) {
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("nested-budget", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		text := messagesText(req)
		if strings.Contains(text, "child-agent") {
			if toolMessages(req) >= 10 {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("child-done")}}}, nil
			}
			return toolCall("child_step", fmt.Sprintf(`{"n":%d}`, toolMessages(req))), nil
		}
		if toolMessages(req) >= 3 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("helper", `{"request":"go"}`), nil
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
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "nest-budget", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	if last.Kind != agent.EventCompleted {
		t.Fatalf("terminal %s %s", last.Kind, eventBlob(events))
	}
}

func TestExceededToolBudgetFailsTheRun(t *testing.T) {
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Context.MaxToolIterations = 2
	cfg.Context.MaxLLMCalls = 20
	mdl := mockmodel.New("over-budget", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if toolMessages(req) > 8 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("stop")}}}, nil
		}
		return toolCall("echo", `{"text":"again"}`), nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "over-budget", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	blob := eventBlob(events)
	if last.Kind != agent.EventFailed {
		t.Fatalf("terminal %s %s", last.Kind, blob)
	}
	if !strings.Contains(blob, "max tool iterations") {
		t.Fatalf("missing budget error: %s", blob)
	}
}

func TestToolStartEndPairs(t *testing.T) {
	t.Run("parallel", func(t *testing.T) {
		cfg := quietConfig(t)
		cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
		var ran atomic.Int32
		work := function.NewFunctionTool(func(ctx context.Context, in struct {
			N int `json:"n"`
		}) (string, error) {
			ran.Add(1)
			return "ok", nil
		}, function.WithName("work"), function.WithDescription("work"))
		mdl := mockmodel.New("pairs", func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if hasToolMessage(req) {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
			calls := make([]model.ToolCall, 5)
			for i := range calls {
				calls[i] = model.ToolCall{
					ID:   fmt.Sprintf("c%d", i),
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "work",
						Arguments: []byte(fmt.Sprintf(`{"n":%d}`, i)),
					},
				}
			}
			return &model.Response{Choices: []model.Choice{{
				Message: model.Message{Role: model.RoleAssistant, ToolCalls: calls},
			}}}, nil
		})
		root := llmagent.New("grape",
			llmagent.WithModel(mdl),
			llmagent.WithEnableParallelTools(true),
			llmagent.WithEnableCodeExecutionResponseProcessor(false),
			llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
			llmagent.WithTools([]tool.Tool{work}),
		)
		rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), FixedAgent: root})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "pairs", Message: "go"})
		if err != nil {
			t.Fatal(err)
		}
		events := collect(t, h)
		if ran.Load() != 5 {
			t.Fatalf("ran %d %s", ran.Load(), eventBlob(events))
		}
		assertToolPairs(t, events, []string{"c0", "c1", "c2", "c3", "c4"})
	})
	t.Run("denied", func(t *testing.T) {
		cfg := quietConfig(t)
		cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
		cfg.Approvals.Required = true
		var ran atomic.Int32
		work := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
			ran.Add(1)
			return "ok", nil
		}, function.WithName("work"), function.WithDescription("work"))
		mdl := mockmodel.New("denied-pairs", func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if hasToolMessage(req) {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
			return toolCallID("work", `{}`, "deny-1"), nil
		})
		rt, err := New(Options{
			Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{work},
			Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
				return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "deny-pair", Message: "go"})
		if err != nil {
			t.Fatal(err)
		}
		events := collect(t, h)
		if ran.Load() != 0 {
			t.Fatalf("denied tool ran %s", eventBlob(events))
		}
		assertToolPairs(t, events, []string{"deny-1"})
	})
	t.Run("nested", func(t *testing.T) {
		cfg := quietConfig(t)
		cfg.Workspace.Permission = agent.PermissionReadOnly
		var ran atomic.Int32
		work := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
			ran.Add(1)
			return "ran", nil
		}, function.WithName("child_work"), function.WithDescription("work"))
		mdl := mockmodel.New("nested-pairs", func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if strings.Contains(messagesText(req), "child-agent") {
				if hasToolMessage(req) || strings.Contains(messagesText(req), "child_work") {
					return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("child-done")}}}, nil
				}
				return toolCallID("child_work", `{}`, "nest-1"), nil
			}
			if hasToolMessage(req) {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
			return toolCallID("helper", `{"request":"go"}`, "help-1"), nil
		})
		cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
		cfg.Approvals.Required = true
		rt, err := New(Options{
			Config: &cfg, Model: mdl, DataDir: t.TempDir(),
			Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
				if req.Detail == "helper" {
					return agent.Decision{ID: req.ID, Allow: true}, nil
				}
				return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
			}),
			Subagents: []SubagentSpec{{
				Name: "helper", Instruction: "child-agent", Model: mdl,
				Tools: []tool.Tool{work},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "nest-pair", Message: "go"})
		if err != nil {
			t.Fatal(err)
		}
		events := collect(t, h)
		blob := eventBlob(events)
		if ran.Load() != 0 {
			t.Fatalf("nested tool ran %s", blob)
		}
		if !strings.Contains(blob, `"detail":"child_work"`) || !strings.Contains(blob, `"allowed":false`) {
			t.Fatalf("child tool was not denied: %s", blob)
		}
		assertToolPairs(t, events, []string{"help-1"})
	})
}

func TestCancelAndIdleReleasePumps(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		cfg := quietConfig(t)
		cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
		cfg.Context.EventIdleTimeout = "5m"
		release := make(chan struct{})
		defer close(release)
		block := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return "late", nil
		}, function.WithName("block"), function.WithDescription("blocks"))
		mdl := mockmodel.New("cancel-pump", func(ctx context.Context, req *model.Request) (*model.Response, error) {
			if hasToolMessage(req) || ctx.Err() != nil {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
			return toolCall("block", `{}`), nil
		})
		rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{block}})
		if err != nil {
			t.Fatal(err)
		}
		defer rt.Close()
		before := runtime.NumGoroutine()
		ctx, cancel := context.WithCancel(context.Background())
		h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "cancel-pump", Message: "go"})
		if err != nil {
			t.Fatal(err)
		}
		<-h.Events()
		cancel()
		deadline := time.Now().Add(2 * time.Second)
		var last agent.Event
		gotTerm := false
		for time.Now().Before(deadline) {
			select {
			case evt, ok := <-h.Events():
				if !ok {
					gotTerm = last.Terminal()
					goto released
				}
				last = evt
				if evt.Terminal() {
					gotTerm = true
				}
			case <-time.After(20 * time.Millisecond):
			}
		}
	released:
		waitGoroutines(t, before+8, 2*time.Second)
		if !gotTerm && !last.Terminal() {
			t.Fatalf("missing terminal after cancel, last=%s", last.Kind)
		}
	})
	t.Run("idle", func(t *testing.T) {
		cfg := quietConfig(t)
		cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
		cfg.Context.EventIdleTimeout = "200ms"
		cfg.Context.EventHoldTimeout = "30s"
		mdl := mockmodel.New("idle-pump", mockmodel.Echo)
		rt := newTestRuntime(t, mdl, cfg)
		defer rt.Close()
		runtime.GC()
		beforeG := runtime.NumGoroutine()
		before := runtimeMem()
		for i := 0; i < 20; i++ {
			h, err := rt.Start(context.Background(), agent.RunRequest{
				UserID: "u", SessionID: fmt.Sprintf("idle%d", i), Message: "hi",
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-h.Events():
			case <-time.After(2 * time.Second):
				t.Fatal("no event")
			}
		}
		time.Sleep(700 * time.Millisecond)
		waitGoroutines(t, beforeG+8, 2*time.Second)
		runtime.GC()
		if after := runtimeMem(); after > before+8<<20 {
			t.Fatalf("heap grew %d", after-before)
		}
	})
}

func quietConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	return cfg
}

func toolMessages(req *model.Request) int {
	if req == nil {
		return 0
	}
	n := 0
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			n++
		}
	}
	return n
}

func toolMessagesSeen(events []agent.Event) int {
	n := 0
	for _, evt := range events {
		if evt.Kind == agent.EventToolEnd {
			n++
		}
	}
	return n
}

func assertToolPairs(t *testing.T, events []agent.Event, ids []string) {
	t.Helper()
	starts := map[string]int{}
	ends := map[string]int{}
	for _, evt := range events {
		if evt.Kind != agent.EventToolStart && evt.Kind != agent.EventToolEnd {
			continue
		}
		var body agent.ToolPayload
		_ = json.Unmarshal(evt.Payload, &body)
		id := body.CallID
		if id == "" {
			id = evt.ToolCallID
		}
		if evt.Kind == agent.EventToolStart {
			starts[id]++
		} else {
			ends[id]++
		}
	}
	for _, id := range ids {
		if starts[id] != 1 || ends[id] != 1 {
			t.Fatalf("id %s start=%d end=%d %s", id, starts[id], ends[id], eventBlob(events))
		}
	}
}

func waitGoroutines(t *testing.T, limit int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for runtime.NumGoroutine() > limit && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
	}
	if runtime.NumGoroutine() > limit {
		t.Fatalf("goroutines %d limit %d", runtime.NumGoroutine(), limit)
	}
}
