package trpcruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/memory"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/evolution"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestPlanModeDenialSkipsCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionDangerFullAccess
	cfg.Workspace.Root = dir
	cfg.Approvals.Required = false
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("plan", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("stopped")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-plan",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "run_command",
						Arguments: []byte(`{"argv":["sh","-c","echo planned > planned.txt"]}`),
					},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mdl,
		DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			return agent.Decision{ID: req.ID, Allow: false, Reason: "plan"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "plan", Workspace: dir, Message: "edit", Mode: agent.ModePlan,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if !hasKind(events, agent.EventPermissionRequest) {
		t.Fatalf("events %v", kinds(events))
	}
	if _, err := os.Stat(filepath.Join(dir, "planned.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("plan denial still ran the command")
	}
}

func TestGoalModeRegistersExtensionAndDefaultDoesNot(t *testing.T) {
	cfg := testConfig(t)
	cfg.Goal.MaxIterations = 1
	cfg.Journal.Enabled = false
	goalModel := mockmodel.New("goal", mockmodel.Echo)
	rt, err := New(Options{Config: &cfg, Model: goalModel, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "goal", Message: "ship it", Mode: agent.ModeGoal,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if events[len(events)-1].Kind != agent.EventCompleted && events[len(events)-1].Kind != agent.EventFailed {
		t.Fatalf("last %s", events[len(events)-1].Kind)
	}
	if !requestHasTool(goalModel, "create_goal") {
		t.Fatal("goal mode did not register create_goal")
	}
	plain := mockmodel.New("plain", mockmodel.Echo)
	rt2 := newTestRuntime(t, plain, testConfig(t))
	defer rt2.Close()
	h, err = rt2.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "plain", Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if requestHasTool(plain, "create_goal") {
		t.Fatal("default mode registered goal tools")
	}
}

func TestSubagentToolUsesIsolatedReply(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("parent", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		last := lastUserText(req)
		if strings.Contains(last, "child-task") {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("child-ok")}}}, nil
		}
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("parent-done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-helper",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "helper",
						Arguments: []byte(`{"request":"child-task"}`),
					},
				}},
			},
		}}}, nil
	})
	child := llmagent.New("helper",
		llmagent.WithModel(mdl),
		llmagent.WithInstruction("answer the request"),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
	)
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), SubAgents: []trpcagent.Agent{child}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "sub", Message: "please delegate"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	blob := ""
	for _, evt := range events {
		blob += string(evt.Payload)
	}
	if !strings.Contains(blob, "child-ok") && !strings.Contains(blob, "parent-done") {
		t.Fatalf("events %s", blob)
	}
}

func TestMemoryStaysOutUntilEnabled(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(filepath.Join(dir, "memory"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("practices/note.md", "purple-hedgehog prefers small patches"); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Memory.Root = store.Root()
	mdl := mockmodel.New("mem", mockmodel.Echo)
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "mem", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if strings.Contains(messagesText(mdl.Requests()[0]), "purple-hedgehog") {
		t.Fatal("default run injected memory")
	}
	if rt.ModelCalls() != mdl.Calls() {
		t.Fatalf("calls runtime %d model %d", rt.ModelCalls(), mdl.Calls())
	}
	cfg.Memory.Enabled = true
	recall := mockmodel.New("recall", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("recalled")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-memory",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "memory_recall",
						Arguments: []byte(`{"query":"purple-hedgehog"}`),
					},
				}},
			},
		}}}, nil
	})
	rt2, err := New(Options{Config: &cfg, Model: recall, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	h, err = rt2.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "mem2", Message: "recall"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	blob := ""
	for _, evt := range events {
		blob += string(evt.Payload)
	}
	if !strings.Contains(blob, "purple-hedgehog") {
		t.Fatalf("recall missed the note: %s", blob)
	}
}

func TestEvolutionOffDoesNotAddModelCalls(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("evo", mockmodel.Echo)
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	if rt.EvolutionEnabled() {
		t.Fatal("evolution defaulted on")
	}
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "evo", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if rt.ModelCalls() != 1 {
		t.Fatalf("model calls %d", rt.ModelCalls())
	}
	var reviews atomic.Int64
	cfg.Evolution.Enabled = true
	rt2, err := New(Options{
		Config:            &cfg,
		Model:             mdl,
		DataDir:           t.TempDir(),
		EvolutionReviewer: countingReviewer{calls: &reviews},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	if !rt2.EvolutionEnabled() {
		t.Fatal("evolution did not start")
	}
	before := mdl.Calls()
	h, err = rt2.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "evo2", Message: "again"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if mdl.Calls()-before != 1 {
		t.Fatalf("enabled evolution changed the user-turn model calls: before %d after %d", before, mdl.Calls())
	}
}

func TestGraphAndTaskHost(t *testing.T) {
	graphAgent, err := UpperGraph()
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("unused", mockmodel.Echo)
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), FixedAgent: graphAgent})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "graph", Message: "Hello"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if !events[len(events)-1].Terminal() {
		t.Fatal("graph run did not finish")
	}
	userCtx, userCancel := context.WithCancel(context.Background())
	userCancel()
	if userCtx.Err() == nil {
		t.Fatal("user context did not cancel")
	}
	started := make(chan context.Context, 1)
	block := mockmodel.New("block", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		started <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	})
	cfg2 := testConfig(t)
	cfg2.Journal.Enabled = false
	rt2, err := New(Options{Config: &cfg2, Model: block, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt2.Close()
	host2, err := NewTaskHost(rt2)
	if err != nil {
		t.Fatal(err)
	}
	task, err := host2.Start(agent.RunRequest{UserID: "u", SessionID: "task", Message: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	var taskCtx context.Context
	select {
	case taskCtx = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not start")
	}
	if taskCtx.Err() != nil {
		t.Fatal("hosted task observed a cancelled context")
	}
	host2.Cancel(task.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, ok := host2.Get(task.ID)
		if ok && (got.Status == "cancelled" || got.Status == "failed") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := host2.Get(task.ID)
	t.Fatalf("task status %#v", got)
}

type countingReviewer struct {
	calls *atomic.Int64
}

func (c countingReviewer) Review(context.Context, *evolution.ReviewInput) (*evolution.ReviewDecision, error) {
	c.calls.Add(1)
	return &evolution.ReviewDecision{SkipReason: "test"}, nil
}

func hasToolMessage(req *model.Request) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			return true
		}
	}
	return false
}

func lastUserText(req *model.Request) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == model.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

func requestHasTool(mdl *mockmodel.Model, name string) bool {
	for _, req := range mdl.Requests() {
		if req == nil {
			continue
		}
		if _, ok := req.Tools[name]; ok {
			return true
		}
	}
	return false
}
