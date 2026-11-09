package trpcruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
