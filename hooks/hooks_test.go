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

type scriptExec struct {
	out   string
	code  int
	block bool
}

func (s *scriptExec) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{}, nil
}

func (s *scriptExec) Execute(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	if s.block {
		<-ctx.Done()
		return sandbox.Result{}, ctx.Err()
	}
	return sandbox.Result{Stdout: s.out, ExitCode: s.code, Isolation: sandbox.IsolationNone}, nil
}

func TestHookTimeoutExitAndMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	slow := NewRunner(&scriptExec{block: true}, []Spec{{
		Event: BeforeTool, Command: "sleep", Blocking: true, Timeout: 150 * time.Millisecond,
	}}, 1)
	start := time.Now()
	if _, err := slow.Run(context.Background(), BeforeTool, dir, `{}`); err == nil {
		t.Fatal("expected timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout did not fire")
	}
	blocking := NewRunner(&scriptExec{code: 2, out: "nope"}, []Spec{{
		Event: BeforeTool, Command: "false", Blocking: true,
	}}, 1)
	results, err := blocking.Run(context.Background(), BeforeTool, dir, `{}`)
	if err == nil || len(results) != 1 || !results[0].Blocked || results[0].ExitCode != 2 {
		t.Fatalf("err=%v results=%#v", err, results)
	}
	open := NewRunner(&scriptExec{code: 2, out: "nope"}, []Spec{{
		Event: BeforeTool, Command: "false", Blocking: false,
	}}, 1)
	results, err = open.Run(context.Background(), BeforeTool, dir, `{}`)
	if err != nil || len(results) != 1 || results[0].Blocked || results[0].ExitCode != 2 {
		t.Fatalf("non-blocking err=%v results=%#v", err, results)
	}
	bad := NewRunner(&scriptExec{out: "{not-json"}, []Spec{{
		Event: BeforeTool, Command: "emit", Blocking: true, ExpectJSON: true,
	}}, 1)
	if _, err := bad.Run(context.Background(), BeforeTool, dir, `{}`); err == nil {
		t.Fatal("expected malformed JSON error")
	}
	if _, err := bad.Run(context.Background(), BeforeTool, dir, `not-json`); err == nil {
		t.Fatal("expected malformed input error")
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
