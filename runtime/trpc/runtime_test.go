package trpcruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
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

type echoIn struct {
	Text string `json:"text"`
}

type echoOut struct {
	Text string `json:"text"`
}

func echoTool() tool.Tool {
	return function.NewFunctionTool(
		func(ctx context.Context, in echoIn) (echoOut, error) {
			return echoOut{Text: in.Text}, nil
		},
		function.WithName("echo"),
		function.WithDescription("Echo text"),
	)
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = true
	cfg.Journal.Path = filepath.Join(t.TempDir(), "journal.ndjson")
	cfg.Model.APIKeyEnv = "MODEL_API_KEY"
	return cfg
}

func boolPtr(v bool) *bool { return &v }

func collect(t *testing.T, h agent.RunHandle) []agent.Event {
	t.Helper()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	var out []agent.Event
	for {
		select {
		case evt, ok := <-h.Events():
			if !ok {
				return out
			}
			out = append(out, evt)
			if evt.Terminal() {
				// Drain until close so the test observes channel closure.
				continue
			}
		case <-timer.C:
			t.Fatal("timed out waiting for events")
		}
	}
}

func kinds(events []agent.Event) []agent.EventKind {
	out := make([]agent.EventKind, len(events))
	for i, evt := range events {
		out[i] = evt.Kind
	}
	return out
}

func TestRunEchoToolAndTerminal(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "sk-should-not-leak")
	mdl := mockmodel.New("mock", mockmodel.ToolThenText("echo", `{"text":"sk-should-not-leak"}`, "done"))
	rt := newTestRuntime(t, mdl, testConfig(t))
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	h, err := rt.Start(ctx, agent.RunRequest{
		UserID: "u", SessionID: "s1", Message: "ping",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	joined := strings.Join(kindStrings(kinds(events)), ",")
	if !strings.Contains(joined, string(agent.EventToolStart)) || !strings.Contains(joined, string(agent.EventToolEnd)) {
		t.Fatalf("events %s", joined)
	}
	last := events[len(events)-1]
	if last.Kind != agent.EventCompleted {
		t.Fatalf("last %s payload %s", last.Kind, last.Payload)
	}
	if !monotonic(events) {
		t.Fatalf("sequence %#v", events)
	}
	raw, err := os.ReadFile(rt.Config().Journal.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-should-not-leak") {
		t.Fatalf("journal leaked secret: %s", raw)
	}
	if mdl.Calls() == 0 || rt.ModelCalls() != mdl.Calls() {
		t.Fatalf("calls model=%d runtime=%d", mdl.Calls(), rt.ModelCalls())
	}
}

func TestSessionSurvivesRestart(t *testing.T) {
	cfg := testConfig(t)
	dir := t.TempDir()
	cfg.Session.Path = filepath.Join(dir, "session.db")
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("mock", mockmodel.Echo)
	rt := newTestRuntime(t, mdl, cfg)
	ctx := context.Background()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "persist", Message: "first-turn"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if events[len(events)-1].Kind != agent.EventCompleted {
		t.Fatalf("events %#v", kinds(events))
	}
	list, err := rt.ListSessions(ctx, "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "persist" {
		t.Fatalf("sessions %#v", list)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	mdl2 := mockmodel.New("mock", mockmodel.Echo)
	rt2 := newTestRuntime(t, mdl2, cfg)
	defer rt2.Close()
	h, err = rt2.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "persist", Message: "second-turn"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	reqs := mdl2.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request")
	}
	blob := messagesText(reqs[0])
	if !strings.Contains(blob, "first-turn") || !strings.Contains(blob, "second-turn") {
		t.Fatalf("history was not restored: %s", blob)
	}
}

func TestCancelClosesChannel(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("mock", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	ctx := context.Background()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "cancel", Message: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := h.Cancel(); err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if len(events) == 0 || !events[len(events)-1].Terminal() {
		t.Fatalf("events %#v", kinds(events))
	}
	last := events[len(events)-1].Kind
	if last != agent.EventCancelled && last != agent.EventFailed {
		t.Fatalf("last %s", last)
	}
}

func TestCancelUnblocksSameSession(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Context.MaxLLMCalls = 1000
	cfg.Context.MaxToolIterations = 1000
	var phase atomic.Int32
	mdl := mockmodel.New("flood", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if phase.Load() == 0 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &model.Response{Choices: []model.Choice{{
				Message: model.Message{
					Role: model.RoleAssistant,
					ToolCalls: []model.ToolCall{{
						ID:   "call-flood",
						Type: "function",
						Function: model.FunctionDefinitionParam{
							Name:      "echo",
							Arguments: []byte(`{"text":"x"}`),
						},
					}},
				},
			}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	before := goruntime.NumGoroutine()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "again", Message: "flood"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if err := h.Cancel(); err != nil {
		t.Fatal(err)
	}
	phase.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	h2, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "again", Message: "next"})
	if err != nil {
		t.Fatalf("same session did not start after cancel: %v", err)
	}
	events := collect(t, h2)
	if time.Since(start) > 3*time.Second {
		t.Fatal("restart exceeded the bound")
	}
	if len(events) == 0 || !events[len(events)-1].Terminal() {
		t.Fatalf("second run %#v", kinds(events))
	}
	deadline := time.Now().Add(2 * time.Second)
	for goruntime.NumGoroutine() > before+12 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if goruntime.NumGoroutine() > before+12 {
		t.Fatalf("goroutines %d baseline %d", goruntime.NumGoroutine(), before)
	}
}

func TestCancelDoesNotOverlapToolExecution(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	release := make(chan struct{})
	var current atomic.Int32
	var overlapped atomic.Bool
	blocker := function.NewFunctionTool(func(ctx context.Context, in echoIn) (echoOut, error) {
		if current.Add(1) > 1 {
			overlapped.Store(true)
		}
		defer current.Add(-1)
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		return echoOut{Text: in.Text}, nil
	}, function.WithName("block"), function.WithDescription("blocks"))
	mdl := mockmodel.New("block", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-block",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "block",
						Arguments: []byte(`{"text":"x"}`),
					},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{blocker}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	defer close(release)
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "overlap", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for current.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if current.Load() == 0 {
		t.Fatal("tool did not start")
	}
	if err := h.Cancel(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(6 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "overlap", Message: "next"})
	if err == nil {
		t.Fatal("second run started while the first tool was still executing")
	}
	if !strings.Contains(err.Error(), "finishing tool execution") {
		t.Fatalf("error %v", err)
	}
	if overlapped.Load() {
		t.Fatal("tool executions overlapped")
	}
}

func TestPoisonedSessionForceRelease(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.ToolDrainTimeout = "200ms"
	release := make(chan struct{})
	defer close(release)
	blocker := function.NewFunctionTool(func(ctx context.Context, in echoIn) (echoOut, error) {
		<-release
		return echoOut{Text: in.Text}, nil
	}, function.WithName("block"), function.WithDescription("blocks"))
	var phase atomic.Int32
	mdl := mockmodel.New("poison", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if phase.Load() > 0 || hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-block",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "block",
						Arguments: []byte(`{"text":"x"}`),
					},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{blocker}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "poison", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := h.Cancel(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var nextErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, nextErr = rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "poison", Message: "next"})
		cancel()
		if nextErr != nil && strings.Contains(nextErr.Error(), "poisoned") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if nextErr == nil || !strings.Contains(nextErr.Error(), "poisoned") {
		t.Fatalf("error %v", nextErr)
	}
	if err := rt.ForceReleaseSession("poison"); err != nil {
		t.Fatal(err)
	}
	phase.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h2, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "poison", Message: "next"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h2)
	if len(events) == 0 || !events[len(events)-1].Terminal() {
		t.Fatalf("recovered run %#v", kinds(events))
	}
}

func TestStaleDrainDoesNotReleaseNextRun(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.ToolDrainTimeout = "5s"
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	defer close(release2)
	var phase atomic.Int32
	var running atomic.Int32
	blocker := function.NewFunctionTool(func(ctx context.Context, in echoIn) (echoOut, error) {
		running.Add(1)
		defer running.Add(-1)
		if phase.Load() == 0 {
			<-release1
		} else {
			<-release2
		}
		return echoOut{Text: in.Text}, nil
	}, function.WithName("block"), function.WithDescription("blocks"))
	mdl := mockmodel.New("stale", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-block",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "block",
						Arguments: []byte(`{"text":"x"}`),
					},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{blocker}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	start := func(msg string) agent.RunHandle {
		t.Helper()
		h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "same", Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	waitRunning := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for running.Load() < n && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if running.Load() < n {
			t.Fatalf("running %d want %d", running.Load(), n)
		}
	}
	h1 := start("one")
	waitRunning(1)
	if err := h1.Cancel(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := rt.ForceReleaseSession("same"); err != nil {
		t.Fatal(err)
	}
	phase.Store(1)
	h2 := start("two")
	waitRunning(2)
	if err := h2.Cancel(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	close(release1)
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "same", Message: "four"})
	if err == nil || !strings.Contains(err.Error(), "finishing") {
		t.Fatalf("later run was not kept out: %v", err)
	}
	if running.Load() != 1 {
		t.Fatalf("running %d", running.Load())
	}
}

func TestAbandonedHandlesReleasePumps(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("abandon", mockmodel.Echo)
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	before := goruntime.NumGoroutine()
	for i := 0; i < 5; i++ {
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: fmt.Sprintf("ab%d", i), Message: "hi",
		})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := h.(*runHandle).Close(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for goruntime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if goruntime.NumGoroutine() > before+4 {
		t.Fatalf("goroutines %d baseline %d", goruntime.NumGoroutine(), before)
	}
}

func TestRuntimeCloseReleasesPumps(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Context.EventHoldTimeout = "30s"
	rt := newTestRuntime(t, mockmodel.New("rt-close", mockmodel.Echo), cfg)
	before := goruntime.NumGoroutine()
	for i := 0; i < 8; i++ {
		if _, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: fmt.Sprintf("rtc%d", i), Message: "hi",
		}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(30 * time.Millisecond)
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for goruntime.NumGoroutine() > before+6 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if goruntime.NumGoroutine() > before+6 {
		t.Fatalf("goroutines %d baseline %d", goruntime.NumGoroutine(), before)
	}
}

func TestAbandonedRunsReleaseWithoutClose(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.EventHoldTimeout = "150ms"
	cfg.Context.MaxLLMCalls = 30
	cfg.Context.MaxToolIterations = 30
	payload := strings.Repeat("x", 4096)
	var n atomic.Int32
	mdl := mockmodel.New("leak", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if ctx.Err() != nil || (hasToolMessage(req) && n.Load() >= 20) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		n.Add(1)
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-leak",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "echo",
						Arguments: []byte(`{"text":"` + payload + `"}`),
					},
				}},
			},
		}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	goruntime.GC()
	beforeG := goruntime.NumGoroutine()
	before := runtimeMem()
	for i := 0; i < 25; i++ {
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: fmt.Sprintf("leak%d", i), Message: "go",
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = h
	}
	time.Sleep(700 * time.Millisecond)
	goruntime.GC()
	goruntime.GC()
	deadline := time.Now().Add(2 * time.Second)
	for goruntime.NumGoroutine() > beforeG+8 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		goruntime.GC()
	}
	if goruntime.NumGoroutine() > beforeG+8 {
		t.Fatalf("goroutines %d baseline %d", goruntime.NumGoroutine(), beforeG)
	}
	after := runtimeMem()
	if after > before+8<<20 {
		t.Fatalf("heap grew %d bytes", after-before)
	}
}

func runtimeMem() uint64 {
	goruntime.GC()
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return m.HeapAlloc
}

func TestSlowLiveReaderKeepsToolEvents(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	// Hold is 300ms and the reader pauses 400ms after every tool_end.
	// A clock that keeps running from run completion drops the rest.
	// The first take cancels that clock, so all 20 tool_end events arrive.
	cfg.Context.EventHoldTimeout = "300ms"
	cfg.Context.EventIdleTimeout = "2s"
	cfg.Context.MaxLLMCalls = 80
	cfg.Context.MaxToolIterations = 80
	var calls atomic.Int32
	mdl := mockmodel.New("slow-reader", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if calls.Add(1) > 20 {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   fmt.Sprintf("call-%d", calls.Load()),
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "echo",
						Arguments: []byte(`{"text":"e"}`),
					},
				}},
			},
		}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "slow", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	ends := 0
	var last agent.Event
	for evt := range h.Events() {
		last = evt
		if evt.Kind != agent.EventToolEnd {
			continue
		}
		ends++
		time.Sleep(400 * time.Millisecond)
	}
	if ends != 20 || !last.Terminal() {
		t.Fatalf("tool_end=%d last=%s", ends, last.Kind)
	}
}

func TestLateReaderAfterHoldSeesTerminal(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Context.EventHoldTimeout = "200ms"
	rt := newTestRuntime(t, mockmodel.New("late-hold", mockmodel.Echo), cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "late-hold", Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	var last agent.Event
	n := 0
	for evt := range h.Events() {
		n++
		last = evt
	}
	if n == 0 || !last.Terminal() {
		t.Fatalf("events=%d last=%s", n, last.Kind)
	}
}

func TestRangeEventsSurvivesGC(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	old := debug.SetGCPercent(1)
	defer debug.SetGCPercent(old)
	rt := newTestRuntime(t, mockmodel.New("gc", mockmodel.Echo), cfg)
	defer rt.Close()
	for i := 0; i < 3; i++ {
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: fmt.Sprintf("gc%d", i), Message: "hello",
		})
		if err != nil {
			t.Fatal(err)
		}
		n, last := consumeWithGC(h.Events())
		if n < 1 || !last.Terminal() {
			t.Fatalf("run %d events=%d last=%s", i, n, last.Kind)
		}
	}
}

func consumeWithGC(events <-chan agent.Event) (int, agent.Event) {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				goruntime.GC()
			}
		}
	}()
	var last agent.Event
	n := 0
	for evt := range events {
		n++
		last = evt
	}
	return n, last
}

func TestCancelDuringToolStartNoRace(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	var current atomic.Int32
	var overlapped atomic.Bool
	release := make(chan struct{})
	defer close(release)
	blocker := function.NewFunctionTool(func(ctx context.Context, in echoIn) (echoOut, error) {
		if current.Add(1) > 1 {
			overlapped.Store(true)
		}
		defer current.Add(-1)
		timer := time.NewTimer(30 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-release:
		case <-timer.C:
		case <-ctx.Done():
		}
		return echoOut{Text: in.Text}, nil
	}, function.WithName("block"), function.WithDescription("blocks briefly"))
	mdl := mockmodel.New("start-race", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-block",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "block",
						Arguments: []byte(`{"text":"x"}`),
					},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{blocker}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for i := 0; i < 50; i++ {
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: "race", Message: "go",
		})
		if err != nil {
			t.Fatalf("iter %d start: %v", i, err)
		}
		go func() { _ = h.Cancel() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		deadline := time.Now().Add(2 * time.Second)
		var nextErr error
		for time.Now().Before(deadline) {
			_, nextErr = rt.Start(ctx, agent.RunRequest{
				UserID: "u", SessionID: "race", Message: "next",
			})
			if nextErr == nil || !strings.Contains(nextErr.Error(), "finishing tool execution") {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		if nextErr != nil && !strings.Contains(nextErr.Error(), "finishing tool execution") && ctx.Err() == nil {
			t.Fatalf("iter %d: %v", i, nextErr)
		}
		// Drain the first handle so its goroutines can exit.
		go func(h agent.RunHandle) {
			for range h.Events() {
			}
		}(h)
		if overlapped.Load() {
			t.Fatalf("iter %d overlapped", i)
		}
	}
}

func TestEventBoxDropsOnlyTextDeltas(t *testing.T) {
	box := newEventBox(4)
	for i := 0; i < 10; i++ {
		box.push(agent.Event{Kind: agent.EventTextDelta, Payload: agent.MustPayload(map[string]int{"i": i})})
	}
	box.push(agent.Event{Kind: agent.EventToolEnd, Payload: agent.MustPayload(map[string]string{"name": "echo"})})
	box.push(agent.Event{Kind: agent.EventPermissionRequest, Payload: agent.MustPayload(map[string]string{"kind": "write"})})
	box.push(agent.Event{Kind: agent.EventError, Payload: agent.MustPayload(map[string]string{"hook": "BeforeTool"})})
	box.push(agent.Event{Kind: agent.EventCompleted, Payload: agent.MustPayload(map[string]string{"status": "completed"})})
	box.close()
	out := make(chan agent.Event)
	go box.pump(out, nil, context.Background(), time.Hour, time.Hour)
	var kinds []agent.EventKind
	var dropped int
	for evt := range out {
		kinds = append(kinds, evt.Kind)
		if strings.Contains(string(evt.Payload), "dropped") {
			dropped++
		}
		if evt.Kind == agent.EventToolEnd || evt.Kind == agent.EventPermissionRequest || evt.Kind == agent.EventError {
			if strings.Contains(string(evt.Payload), "dropped") && evt.Kind != agent.EventRaw {
				t.Fatalf("important event replaced: %s", evt.Payload)
			}
		}
	}
	if dropped == 0 {
		t.Fatalf("no drop marker in %#v", kinds)
	}
	for _, kind := range []agent.EventKind{agent.EventToolEnd, agent.EventPermissionRequest, agent.EventError, agent.EventCompleted} {
		found := false
		for _, got := range kinds {
			if got == kind {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s in %#v", kind, kinds)
		}
	}
}

func TestSlowConsumerKeepsToolEvents(t *testing.T) {
	const calls = 400
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.MaxLLMCalls = calls + 5
	cfg.Context.MaxToolIterations = calls + 5
	var n atomic.Int32
	mdl := mockmodel.New("keep", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if hasToolMessage(req) && n.Load() >= calls {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		n.Add(1)
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-keep",
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
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "keep", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for n.Load() < calls && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	events := collect(t, h)
	var tools int
	for _, evt := range events {
		if evt.Kind == agent.EventToolEnd || evt.Kind == agent.EventToolStart {
			tools++
		}
	}
	if tools < calls {
		t.Fatalf("kept %d tool events, want at least %d (len %d)", tools, calls, len(events))
	}
	if !events[len(events)-1].Terminal() {
		t.Fatalf("last %s", events[len(events)-1].Kind)
	}
}

func TestLateConsumerReceivesTerminalEvent(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Context.MaxLLMCalls = 800
	cfg.Context.MaxToolIterations = 800
	var calls atomic.Int32
	mdl := mockmodel.New("flood2", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if hasToolMessage(req) {
			if calls.Load() > 300 {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
		}
		calls.Add(1)
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-flood",
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
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "late", Message: "flood"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for calls.Load() < 256 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 256 {
		t.Fatalf("produced %d events before reading", calls.Load())
	}
	events := collect(t, h)
	if len(events) == 0 || !events[len(events)-1].Terminal() {
		t.Fatalf("late consumer last=%v len=%d", kinds(events), len(events))
	}
}

func TestRejectConcurrentSession(t *testing.T) {
	cfg := testConfig(t)
	cfg.Journal.Enabled = false
	started := make(chan struct{})
	release := make(chan struct{})
	mdl := mockmodel.New("mock", func(ctx context.Context, req *model.Request) (*model.Response, error) {
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
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("ok")}}}, nil
	})
	rt, err := New(Options{
		Config:                  &cfg,
		Model:                   mdl,
		DataDir:                 t.TempDir(),
		Tools:                   []tool.Tool{echoTool()},
		RejectConcurrentSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx := context.Background()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "same", Message: "one"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not start")
	}
	_, err = rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "same", Message: "two"})
	if err == nil || !errors.Is(err, agent.ErrSession) {
		t.Fatalf("err=%v", err)
	}
	close(release)
	_ = collect(t, h)
}

func TestExternalModuleBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	root := moduleRoot(t)
	dir := t.TempDir()
	mod := `module example.com/grape-consumer

go 1.27.0

require github.com/lookerjin/grape-core v0.0.0

replace github.com/lookerjin/grape-core => ` + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	src := `package main
import (
  "fmt"
  "github.com/lookerjin/grape-core/agent"
  "github.com/lookerjin/grape-core/config"
  trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
)
func main() {
  _ = agent.ModeDefault
  cfg := config.Default()
  _ = trpcruntime.Options{Config: &cfg}
  fmt.Println(cfg.Runtime.Provider)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "GOTOOLCHAIN=local", "CGO_ENABLED=1")
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Env = env
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("tidy: %v\n%s", err, out)
	}
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "consumer"), ".")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
}

func TestDomainPackagesDoNotImportTRPC(t *testing.T) {
	for _, pkg := range []string{
		"github.com/lookerjin/grape-core/agent",
		"github.com/lookerjin/grape-core/config",
		"github.com/lookerjin/grape-core/coding",
		"github.com/lookerjin/grape-core/sandbox",
		"github.com/lookerjin/grape-core/plugin",
		"github.com/lookerjin/grape-core/mcp",
		"github.com/lookerjin/grape-core/skill",
		"github.com/lookerjin/grape-core/hooks",
		"github.com/lookerjin/grape-core/rules",
		"github.com/lookerjin/grape-core/memory",
	} {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", pkg)
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", pkg, err, out)
		}
		if strings.Contains(string(out), "trpc.group") {
			t.Fatalf("%s imports trpc\n%s", pkg, out)
		}
	}
}

func newTestRuntime(t *testing.T, mdl *mockmodel.Model, cfg config.Config) *Runtime {
	t.Helper()
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mdl,
		DataDir: t.TempDir(),
		Tools:   []tool.Tool{echoTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func monotonic(events []agent.Event) bool {
	var prev uint64
	for _, evt := range events {
		if evt.Sequence <= prev {
			return false
		}
		prev = evt.Sequence
	}
	return true
}

func kindStrings(kinds []agent.EventKind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

func messagesText(req *model.Request) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range req.Messages {
		b.WriteString(string(msg.Role))
		b.WriteString(":")
		b.WriteString(msg.Content)
		b.WriteString("\n")
	}
	return b.String()
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
