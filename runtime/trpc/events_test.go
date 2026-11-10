package trpcruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

func TestFullStreamHasOneTerminal(t *testing.T) {
	cfg := quietConfig(t)
	mdl := mockmodel.New("one", mockmodel.Echo)
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "one-terminal", Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	n := 0
	for _, evt := range events {
		if evt.Terminal() {
			n++
		}
	}
	if n != 1 || !events[len(events)-1].Terminal() {
		t.Fatalf("terminals %d last %s", n, events[len(events)-1].Kind)
	}
	status := h.Status()
	if status.Status != string(events[len(events)-1].Kind) || status.Cause != "" {
		t.Fatalf("status %#v", status)
	}
}

func TestCloseWithoutTerminalReportsDelivery(t *testing.T) {
	cfg := quietConfig(t)
	started := make(chan struct{})
	release := make(chan struct{})
	mdl := mockmodel.New("hold", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "delivery", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				goto closed
			}
		case <-timer.C:
			t.Fatal("events did not close")
		}
	}
closed:
	status := h.Status()
	if status.Status != string(agent.EventCancelled) || status.Cause != agent.CauseDelivery {
		t.Fatalf("status %#v", status)
	}
	close(release)
	_ = h.Cancel()
}

func TestToolErrorDoesNotFailTheRun(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	boom := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
		return "", errors.New("tool broke")
	}, function.WithName("boom"), function.WithDescription("fails"))
	mdl := mockmodel.New("recover", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("boom", `{}`), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{boom}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "recover", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	last := events[len(events)-1]
	if last.Kind != agent.EventCompleted {
		t.Fatalf("terminal %s %s", last.Kind, eventBlob(events))
	}
	if h.Status().Status != string(agent.EventCompleted) || h.Status().Reason != "" {
		t.Fatalf("status %#v", h.Status())
	}
}

func TestEmptyToolCallIDsStayDistinct(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	var round int
	mdl := mockmodel.New("ids", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		round++
		if round > 2 || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "echo",
						Arguments: []byte(`{"text":"x"}`),
					},
				}},
			},
		}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "ids", Message: "go"})
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
		if body.CallID == "" || !strings.Contains(body.CallID, "trpc-agent-go-toolcall:") {
			t.Fatalf("call id %q", body.CallID)
		}
		if evt.Kind == agent.EventToolStart {
			starts[body.CallID]++
		} else {
			ends[body.CallID]++
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("starts %v ends %v", starts, ends)
	}
	for id, n := range starts {
		if n != 1 || ends[id] != 1 {
			t.Fatalf("id %s start %d end %d", id, n, ends[id])
		}
	}
}

func TestTrustedAgentToolStreamsChildEvents(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	mdl := mockmodel.New("stream", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if strings.Contains(messagesText(req), "child-stream") {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("stream-child")}}}, nil
		}
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("parent-done")}}}, nil
		}
		return toolCall("helper", `{"request":"go"}`), nil
	})
	child := llmagent.New("helper",
		llmagent.WithModel(mdl),
		llmagent.WithInstruction("child-stream"),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
	)
	at := agenttool.NewTool(child, agenttool.WithStreamInner(true))
	long := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
		return "ok", nil
	}, function.WithName("long"), function.WithDescription("long"), function.WithLongRunning(true))
	wrapped := trackTool(kindTool(long, "side_effect"))
	if !longRunning(wrapped) {
		t.Fatal("long-running flag was dropped")
	}
	if streamInner(wrapTrusted(at)) != true {
		t.Fatal("trusted wrapper dropped StreamInner")
	}
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		ToolSpecs: []ToolSpec{{Tool: at, Trusted: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "stream-child", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	found := false
	for _, evt := range events {
		if evt.AgentID == "helper" && evt.Kind == agent.EventText && strings.Contains(string(evt.Payload), "stream-child") {
			found = true
		}
	}
	if !found {
		t.Fatalf("child text was not streamed: %s", eventBlob(events))
	}
}
