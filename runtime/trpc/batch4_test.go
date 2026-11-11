package trpcruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

func TestUsersDoNotShareASessionLock(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	release := make(chan struct{})
	defer close(release)
	var entered atomic.Int32
	block := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
		entered.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "ok", nil
	}, function.WithName("block"), function.WithDescription("blocks"))
	var step atomic.Int32
	mdl := mockmodel.New("users", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if step.Add(1) > 2 || hasToolMessage(req) || ctx.Err() != nil {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("block", `{}`), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{block}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	start := func(user string) {
		t.Helper()
		_, err := rt.Start(context.Background(), agent.RunRequest{UserID: user, SessionID: "shared", Message: "go"})
		if err != nil {
			t.Fatal(err)
		}
	}
	start("a")
	deadline := time.Now().Add(2 * time.Second)
	for entered.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start("b")
	deadline = time.Now().Add(2 * time.Second)
	for entered.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if entered.Load() < 2 {
		t.Fatalf("second user blocked, entered=%d", entered.Load())
	}
}

func TestSessionLockMapDropsIdleKeys(t *testing.T) {
	cfg := quietConfig(t)
	mdl := mockmodel.New("idle-lock", mockmodel.Echo)
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	for i := 0; i < 5; i++ {
		h, err := rt.Start(context.Background(), agent.RunRequest{
			UserID: "u", SessionID: "s" + string(rune('a'+i)), Message: "hi",
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = collect(t, h)
	}
	rt.mu.Lock()
	n := len(rt.locks)
	rt.mu.Unlock()
	if n != 0 {
		t.Fatalf("locks left %d", n)
	}
}

func TestForceReleaseRefusesARunningTool(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Context.ToolDrainTimeout = "150ms"
	release := make(chan struct{})
	defer close(release)
	block := function.NewFunctionTool(func(ctx context.Context, in struct{}) (string, error) {
		<-release
		return "ok", nil
	}, function.WithName("block"), function.WithDescription("blocks"))
	mdl := mockmodel.New("refuse", func(ctx context.Context, req *model.Request) (*model.Response, error) {
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
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "busy", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	_ = h.Cancel()
	deadline := time.Now().Add(2 * time.Second)
	var refused error
	for time.Now().Before(deadline) {
		refused = rt.ForceReleaseSession("busy", agent.SessionRelease{UserID: "u"})
		if refused != nil && strings.Contains(refused.Error(), "not safe recovery") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if refused == nil || !strings.Contains(refused.Error(), "not safe recovery") {
		t.Fatalf("release err %v", refused)
	}
}

func TestStreamTrackingLeavesWhenTheReaderEnds(t *testing.T) {
	st := newRunState("r", "s", agent.ModeDefault)
	ctx := withState(context.Background(), st)
	release := make(chan struct{})
	inner := &gateStream{release: release}
	tracked := streamTracking{trackingTool: trackingTool{inner: inner}, stream: inner}
	reader, err := tracked.StreamableCall(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !st.toolBusy() {
		t.Fatal("left when StreamableCall returned")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for st.toolBusy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st.toolBusy() {
		t.Fatal("still held after the producer closed")
	}
	reader.Close()
	if st.toolBusy() {
		t.Fatal("close counted the tool again")
	}

	release = make(chan struct{})
	defer close(release)
	inner = &gateStream{release: release, keepOpen: true}
	tracked = streamTracking{trackingTool: trackingTool{inner: inner}, stream: inner}
	reader, err = tracked.StreamableCall(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for !st.toolBusy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	reader.Close()
	deadline = time.Now().Add(2 * time.Second)
	for st.toolBusy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st.toolBusy() {
		t.Fatal("still held after the reader closed")
	}
}

type gateStream struct {
	release  chan struct{}
	keepOpen bool
}

func (g *gateStream) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "gate", Description: "stream"}
}

func (g *gateStream) Call(context.Context, []byte) (any, error) { return "ok", nil }

func (g *gateStream) StreamableCall(context.Context, []byte) (*tool.StreamReader, error) {
	s := tool.NewStream(1)
	go func() {
		if g.keepOpen {
			for {
				if s.Writer.Send(tool.StreamChunk{Content: "x"}, nil) {
					return
				}
			}
		}
		<-g.release
		s.Writer.Close()
	}()
	return s.Reader, nil
}

func TestTaskReloadAndClose(t *testing.T) {
	cfg := quietConfig(t)
	rt := newTestRuntime(t, mockmodel.New("tasks", mockmodel.Echo), cfg)
	defer rt.Close()
	index := filepath.Join(filepath.Dir(rt.cfg.Session.Path), "tasks.json")
	raw := `{
	  "done": {"id":"done","session_id":"s","user_id":"u","status":"completed"},
	  "live": {"id":"live","session_id":"s","user_id":"u","status":"running"}
	}`
	if err := os.WriteFile(index, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	host, err := NewTaskHost(rt)
	if err != nil {
		t.Fatal(err)
	}
	done, ok := host.Get("done")
	if !ok || done.Status != "completed" {
		t.Fatalf("finished task %#v ok=%v", done, ok)
	}
	got, ok := host.Get("live")
	if !ok || got.Status != "failed" || got.Error != agent.TaskInterrupted {
		t.Fatalf("reloaded %#v ok=%v", got, ok)
	}
	again, err := NewTaskHost(rt)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := again.List()
	if err != nil || len(listed) != 2 || listed[0].ID != "done" || listed[1].Error != agent.TaskInterrupted {
		t.Fatalf("reloaded task list: %+v %v", listed, err)
	}
	if done, ok = again.Get("done"); !ok || done.Status != "completed" {
		t.Fatalf("second reload finished %#v ok=%v", done, ok)
	}
	if got, ok = again.Get("live"); !ok || got.Error != agent.TaskInterrupted {
		t.Fatalf("second reload interrupted %#v ok=%v", got, ok)
	}
	failedStart, err := host.Start(agent.RunRequest{UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		n := len(host.cancel)
		host.mu.Unlock()
		got, ok = host.Get(failedStart.ID)
		if n == 0 && ok && got.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	host.mu.Lock()
	n := len(host.cancel)
	host.mu.Unlock()
	got, _ = host.Get(failedStart.ID)
	if n != 0 || got.Status != "failed" {
		t.Fatalf("start failure cancel=%d task=%#v", n, got)
	}

	started := make(chan struct{})
	block := mockmodel.New("task-block", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	rt2 := newTestRuntime(t, block, quietConfig(t))
	host2, err := NewTaskHost(rt2)
	if err != nil {
		t.Fatal(err)
	}
	task, err := host2.Start(agent.RunRequest{UserID: "u", SessionID: "task-close", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not start")
	}
	if err := rt2.Close(); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, ok = host2.Get(task.ID)
		if ok && (got.Status == "cancelled" || got.Status == "failed") {
			rt2.mu.Lock()
			left := rt2.taskHost
			rt2.mu.Unlock()
			if left != nil {
				t.Fatal("task host still registered")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, _ = host2.Get(task.ID)
	t.Fatalf("task after close %#v", got)
}

func TestSkillSnapshotServesTheDiscoverChoice(t *testing.T) {
	root := t.TempDir()
	writeNamedSkill(t, filepath.Join(root, "a"), "review", "first")
	writeNamedSkill(t, filepath.Join(root, "b"), "review", "second")
	if _, err := newSkillSnapshot("error", []string{filepath.Join(root, "a"), filepath.Join(root, "b")}); err == nil {
		t.Fatal("duplicate name was skipped")
	}
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "broken", "SKILL.md"), []byte("---\nname: [\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newSkillSnapshot("error", []string{bad}); err == nil {
		t.Fatal("parse error was skipped")
	}
	repo, err := newSkillSnapshot("earliest", []string{filepath.Join(root, "a"), filepath.Join(root, "b")})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := repo.Path("review")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "a", "review") {
		t.Fatalf("path %s", dir)
	}
	got, err := repo.Get("review")
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Description != "first" {
		t.Fatalf("description %q", got.Summary.Description)
	}
}

func TestSkillGetUsesTRPCDocLoading(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "review")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\r\nname: review\r\ndescription: crlf\r\n---\r\nBody line.\r\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte("echo hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "img.png"), []byte("not text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "ref.md"), []byte("see this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := newSkillSnapshot("error", []string{root})
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get("review")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got.Body, "\r") || strings.HasPrefix(got.Body, "\n") {
		t.Fatalf("body %q", got.Body)
	}
	if !strings.Contains(got.Body, "Body line.") {
		t.Fatalf("body %q", got.Body)
	}
	if len(got.Docs) != 1 || got.Docs[0].Path != "sub/ref.md" {
		var paths []string
		for _, doc := range got.Docs {
			paths = append(paths, doc.Path)
		}
		t.Fatalf("docs %v", paths)
	}
}

func TestAgentIDUsesTheRunnerName(t *testing.T) {
	cfg := quietConfig(t)
	var calls atomic.Int32
	mdl := mockmodel.New("named", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		calls.Add(1)
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("ok")}}}, nil
	})
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	_, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "missing-agent", Message: "hi", AgentID: "missing"})
	if err == nil {
		t.Fatal("missing agent was ignored")
	}
	if calls.Load() != 0 {
		t.Fatalf("model ran for a missing agent, calls=%d", calls.Load())
	}
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "named-agent", Message: "hi", AgentID: "grape"})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if !events[len(events)-1].Terminal() {
		t.Fatal("named default agent did not finish")
	}
}

func writeNamedSkill(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nBody.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
