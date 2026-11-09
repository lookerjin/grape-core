package hooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/sandbox"
)

type fakeExec struct {
	err   error
	calls int
}

func (f *fakeExec) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{}, nil
}

func (f *fakeExec) Execute(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	f.calls++
	if f.err != nil {
		return sandbox.Result{}, f.err
	}
	return sandbox.Result{ExitCode: 0, Isolation: sandbox.IsolationNone}, nil
}

func TestUnsupportedEvent(t *testing.T) {
	if _, err := Parse([]byte(`{"hooks":[{"event":"OnError","command":"echo"}]}`)); err == nil {
		t.Fatal("expected unsupported")
	}
	r := NewRunner(&fakeExec{}, nil, 1)
	if _, err := r.Run(context.Background(), OnError, t.TempDir(), "{}"); err == nil {
		t.Fatal("expected unsupported run")
	}
}

func TestDenialDoesNotCountAsSuccess(t *testing.T) {
	exec := &fakeExec{err: errors.New("denied")}
	r := NewRunner(exec, []Spec{{
		Event:    BeforeTool,
		Command:  "echo",
		Blocking: true,
		Timeout:  time.Second,
	}}, 1)
	_, err := r.Run(context.Background(), BeforeTool, t.TempDir(), `{"name":"run_command"}`)
	if err == nil {
		t.Fatal("expected denial")
	}
	if exec.calls != 1 {
		t.Fatalf("calls %d", exec.calls)
	}
	if _, err := r.Run(context.Background(), BeforeTool, t.TempDir(), `{"name":"run_command"}`); err != nil {
		t.Fatal(err)
	}
	if exec.calls != 1 {
		t.Fatal("duplicate hook executed")
	}
}
