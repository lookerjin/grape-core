package trpcruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestCommittedForkSideAndRewind(t *testing.T) {
	ctx := context.Background()
	mainEntered := make(chan struct{})
	mainRelease := make(chan struct{})
	sideEntered := make(chan struct{})
	m := mockmodel.New("snapshot", func(ctx context.Context, r *model.Request) (*model.Response, error) {
		var words []string
		for _, msg := range r.Messages {
			if msg.Role == model.RoleUser {
				words = append(words, msg.Content)
			}
		}
		switch words[len(words)-1] {
		case "pending":
			close(mainEntered)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-mainRelease:
			}
		case "cancel side":
			close(sideEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		case "aside":
			if len(r.Tools) > 0 {
				t.Error("side question received tools")
			}
		}
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(strings.Join(words, "|"))}}}, nil
	})
	cfg := testConfig(t)
	dir := t.TempDir()
	open := func() *Runtime {
		rt, e := New(Options{Config: &cfg, Model: m, TurnCheckpoints: true, DataDir: dir})
		if e != nil {
			t.Fatal(e)
		}
		return rt
	}
	rt := open()
	defer func() { rt.Close() }()
	start := func(id, msg string) agent.RunHandle {
		t.Helper()
		h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: id, Message: msg})
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	finish := func(h agent.RunHandle) {
		t.Helper()
		collect(t, h)
		if h.Status().Status != "completed" {
			t.Fatal(h.Status())
		}
	}
	first := start("s", "first")
	finish(first)
	active := start("s", "pending")
	select {
	case <-mainEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("waiting for main")
	}
	a, e := rt.AskSide(ctx, "u", "s", "aside", nil)
	if e != nil || a.Checkpoint != first.ID() || a.Text != "first|aside" {
		t.Fatalf("%+v %v", a, e)
	}
	child, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, e := rt.AskSide(child, "u", "s", "cancel side", nil); done <- e }()
	select {
	case <-sideEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("waiting for side")
	}
	cancel()
	if !errors.Is(<-done, context.Canceled) {
		t.Fatal("side cancellation")
	}
	if _, e := rt.ForkSession(ctx, "other", "s", first.ID()); !errors.Is(e, agent.ErrSession) {
		t.Fatal(e)
	}
	if _, e := rt.ForkSession(ctx, "u", "s", active.ID()); !errors.Is(e, agent.ErrSession) {
		t.Fatal(e)
	}
	fork, e := rt.ForkSession(ctx, "u", "s", first.ID())
	if e != nil {
		t.Fatal(e)
	}
	close(mainRelease)
	finish(active)
	rt.Close()
	rt = open()
	marks, e := rt.Checkpoints(ctx, "u", fork)
	if e != nil || len(marks) != 1 || marks[0].ParentSession != "s" {
		t.Fatal(marks, e)
	}
	finish(start(fork, "branch"))
	if e := rt.RewindSession(ctx, "u", "s", first.ID()); e != nil {
		t.Fatal(e)
	}
	for id, want := range map[string]string{"s": "first|aside", fork: "first|branch|aside"} {
		a, e := rt.AskSide(ctx, "u", id, "aside", nil)
		if e != nil || a.Text != want {
			t.Fatal(id, a, e)
		}
	}
	finish(start("s", "again"))
}
