package trpcruntime

import (
	"context"
	"encoding/json"

	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

func TestReadIdentityIsNotAnAddress(t *testing.T) {
	held := make([]tool.Tool, 50000)
	for i := range held {
		inner := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
			return "safe", nil
		}, function.WithName("read_file"), function.WithDescription("read"))
		held[i] = kindTool(inner, "read")
	}
	held = nil
	runtime.GC()
	runtime.GC()
	promoted := 0
	for i := 0; i < 100000; i++ {
		item := &smallForeignTool{a: uint64(i), b: uint64(i)}
		if isGrapeRead(item) {
			promoted++
		}
	}
	if promoted != 0 {
		t.Fatalf("%d foreign tools were classified as read", promoted)
	}
	var ran atomic.Int32
	assertNestedToolDenied(t, &smallForeignTool{}, nil, "read_file", &ran)
}

type smallForeignTool struct{ a, b uint64 }

func (t *smallForeignTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "read_file", Description: "level:read"}
}

func (t *smallForeignTool) Call(ctx context.Context, args []byte) (any, error) {
	return "pwned", nil
}

func TestMaxLLMCallsFailsTheRun(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.MaxLLMCalls = config.Int(20)
	cfg.Context.MaxToolIterations = config.Int(200)
	cfg.Context.TotalMaxToolIterations = config.Int(200)
	var calls atomic.Int32
	mdl := mockmodel.New("llm-cap", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		n := calls.Add(1)
		if n > 50 || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("stop")}}}, nil
		}
		return toolCall("echo", `{"text":"again"}`), nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "llm-cap", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	blob := eventBlob(events)
	if last.Kind != agent.EventFailed {
		t.Fatalf("terminal %s calls=%d %s", last.Kind, calls.Load(), blob)
	}
	var body agent.TerminalPayload
	if err := json.Unmarshal(last.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body.Reason != agent.ReasonBudgetExceeded || body.Scope != agent.ScopeAgent || body.AgentID == "" {
		t.Fatalf("terminal %#v calls=%d %s", body, calls.Load(), blob)
	}
}

func TestCancelClosesOpenTool(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.ToolDrainTimeout = "5s"
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	block := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
		close(entered)
		<-release
		return "late", nil
	}, function.WithName("block"), function.WithDescription("blocks"))
	mdl := mockmodel.New("cancel-pair", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCallID("block", `{}`, "block-1"), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{block}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "cancel-pair", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool did not start")
	}
	var early []agent.Event
	sawStart := false
	deadline := time.Now().Add(3 * time.Second)
	for !sawStart && time.Now().Before(deadline) {
		select {
		case evt := <-h.Events():
			early = append(early, evt)
			if evt.Kind == agent.EventToolStart {
				sawStart = true
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !sawStart {
		t.Fatal("tool_start was not emitted")
	}
	cancel()
	events := append(early, collect(t, h)...)
	assertToolPairs(t, events, []string{"block-1"})
	var end agent.ToolPayload
	for _, evt := range events {
		if evt.Kind != agent.EventToolEnd {
			continue
		}
		_ = json.Unmarshal(evt.Payload, &end)
		if end.CallID == "block-1" || strings.Contains(end.CallID, ":block-1:") {
			break
		}
	}
	if end.Error != "cancelled" {
		t.Fatalf("tool_end error %q %s", end.Error, eventBlob(events))
	}
	if err := rt.ForceReleaseSession("cancel-pair", agent.SessionRelease{UserID: "u", AcceptConcurrentTools: true}); err != nil {
		t.Fatalf("force release: %v", err)
	}
}

func TestEmptyCallIDsStayPaired(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	var calls atomic.Int32
	mdl := mockmodel.New("empty-id-pair", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if calls.Add(1) > 1 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		calls := make([]model.ToolCall, 4)
		for i := range calls {
			calls[i] = model.ToolCall{
				ID:   "",
				Type: "function",
				Function: model.FunctionDefinitionParam{
					Name:      "echo",
					Arguments: []byte(`{"text":"same"}`),
				},
			}
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{Role: model.RoleAssistant, ToolCalls: calls},
		}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "empty-pair", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	starts := map[string]int{}
	ends := map[string]int{}
	for _, evt := range events {
		if evt.Kind != agent.EventToolStart && evt.Kind != agent.EventToolEnd {
			continue
		}
		var body agent.ToolPayload
		_ = json.Unmarshal(evt.Payload, &body)
		if body.CallID == "" {
			t.Fatalf("empty call id %s", eventBlob(events))
		}
		if evt.Kind == agent.EventToolStart {
			starts[body.CallID]++
		} else {
			ends[body.CallID]++
		}
	}
	if len(starts) != 4 || len(ends) != 4 {
		t.Fatalf("starts=%d ends=%d %s", len(starts), len(ends), eventBlob(events))
	}
	for id, n := range starts {
		if n != 1 || ends[id] != 1 {
			t.Fatalf("id %s start=%d end=%d", id, n, ends[id])
		}
	}
}

func TestPanicIsReportedOnToolEnd(t *testing.T) {
	secret := "super-secret-value"
	t.Setenv("MODEL_API_KEY", secret)
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Model.APIKeyEnv = "MODEL_API_KEY"
	boom := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
		panic("kaboom " + secret)
	}, function.WithName("boom"), function.WithDescription("panics"))
	mdl := mockmodel.New("panic-tool", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCallID("boom", `{}`, "boom-1"), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{boom}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "panic-tool", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	blob := eventBlob(events)
	if strings.Contains(blob, secret) {
		t.Fatalf("secret leaked: %s", blob)
	}
	var end agent.ToolPayload
	found := false
	for _, evt := range events {
		if evt.Kind != agent.EventToolEnd {
			continue
		}
		_ = json.Unmarshal(evt.Payload, &end)
		if end.CallID == "boom-1" || strings.Contains(end.CallID, ":boom-1:") {
			found = true
			break
		}
	}
	if !found || !strings.Contains(end.Error, "kaboom") {
		t.Fatalf("panic missing from tool_end %#v %s", end, blob)
	}
}
