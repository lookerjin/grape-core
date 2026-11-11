package trpcruntime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestFirstTurnRecovery(t *testing.T) {
	if data := os.Getenv("GRAPE_RECOVERY_CRASH_DATA"); data != "" {
		cfg := testConfig(t)
		cfg.Workspace.Root = filepath.Join(data, "workspace")
		cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
		rt, e := New(Options{Config: &cfg, DataDir: data, TurnCheckpoints: true, Model: recoveryFailureModel("crash", nil)})
		if e != nil {
			t.Fatal(e)
		}
		h, e := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "s", Message: "edit"})
		if e != nil {
			t.Fatal(e)
		}
		collect(t, h)
		t.Fatal("child should exit after managed write")
	}
	for _, mode := range []string{"completed", "cancel", "provider-failure", "tool-error", "crash"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			data := t.TempDir()
			ws := filepath.Join(data, "workspace")
			if e := os.Mkdir(ws, 0700); e != nil {
				t.Fatal(e)
			}
			file := filepath.Join(ws, "note")
			if e := os.WriteFile(file, []byte("original"), 0640); e != nil {
				t.Fatal(e)
			}
			cfg := testConfig(t)
			cfg.Workspace.Root = ws
			cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
			if mode == "crash" {
				cmd := exec.Command(os.Args[0], "-test.run=^TestFirstTurnRecovery$")
				cmd.Env = append(os.Environ(), "GRAPE_RECOVERY_CRASH_DATA="+data)
				out, e := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(e, &exit) || exit.ExitCode() != 41 {
					t.Fatalf("child: %v %s", e, out)
				}
			} else {
				written := make(chan struct{})
				rt, e := New(Options{Config: &cfg, DataDir: data, TurnCheckpoints: true, Model: recoveryFailureModel(mode, written)})
				if e != nil {
					t.Fatal(e)
				}
				h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "edit"})
				if e != nil {
					t.Fatal(e)
				}
				if mode == "cancel" {
					select {
					case <-written:
					case <-time.After(5 * time.Second):
						t.Fatal("write did not finish")
					}
					if e = h.Cancel(); e != nil {
						t.Fatal(e)
					}
				}
				collect(t, h)
				want := "failed"
				if mode == "completed" {
					want = "completed"
				}
				if mode == "cancel" {
					want = "cancelled"
				}
				if h.Status().Status != want {
					t.Fatalf("terminal %+v", h.Status())
				}
				rt.Close()
			}
			if b, e := os.ReadFile(file); e != nil || string(b) != "managed" {
				t.Fatalf("actual write missing: %q %v", b, e)
			}
			mdl := mockmodel.New("restart", mockmodel.Echo)
			rt, e := New(Options{Config: &cfg, DataDir: data, TurnCheckpoints: true, Model: mdl})
			if e != nil {
				t.Fatal(e)
			}
			defer rt.Close()
			marks, e := rt.Checkpoints(ctx, "u", "s")
			if e != nil {
				t.Fatal(e)
			}
			want := 1
			if mode == "completed" {
				want = 2
			}
			if len(marks) != want || !marks[0].Baseline || marks[0].RunID != agent.SessionBaselineID {
				t.Fatal(marks)
			}
			child, e := rt.ForkSession(ctx, "u", "s", marks[0].RunID)
			if e != nil {
				t.Fatal(e)
			}
			if h, e := rt.SessionHistory(ctx, "u", child, 0); e != nil || len(h) != 0 {
				t.Fatal(h, e)
			}
			before, e := rt.SessionHistory(ctx, "u", "s", 0)
			if e != nil {
				t.Fatal(e)
			}
			p, e := rt.PreviewFiles(ctx, "u", "s", marks[0].RunID)
			if e != nil || len(p.Files) != 1 || p.Files[0].Conflict != "" {
				t.Fatal(p, e)
			}
			r, e := rt.RestoreFiles(ctx, "u", "s", marks[0].RunID, p.Digest)
			if e != nil || r.Status != "completed" {
				t.Fatal(r, e)
			}
			after, e := rt.SessionHistory(ctx, "u", "s", 0)
			if e != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("file restore changed conversation", e)
			}
			if b, e := os.ReadFile(file); e != nil || string(b) != "original" {
				t.Fatal(string(b), e)
			}
			// An unrelated file conflict must not block conversation-only rewind.
			if e = os.WriteFile(file, []byte("editor"), 0640); e != nil {
				t.Fatal(e)
			}
			if e = rt.RewindSession(ctx, "u", "s", marks[0].RunID); e != nil {
				t.Fatal(e)
			}
			if b, _ := os.ReadFile(file); string(b) != "editor" {
				t.Fatal("conversation rewind changed file")
			}
			if h, e := rt.SessionHistory(ctx, "u", "s", 0); e != nil || len(h) != 0 {
				t.Fatal(h, e)
			}
			h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "new"})
			if e != nil {
				t.Fatal(e)
			}
			collect(t, h)
			if h.Status().Status != "completed" {
				t.Fatal(h.Status())
			}
			for _, request := range mdl.Requests() {
				for _, msg := range request.Messages {
					if msg.Role == model.RoleUser && msg.Content != "new" {
						t.Fatal("discarded context returned", msg.Content)
					}
				}
			}
			marks, e = rt.Checkpoints(ctx, "u", "s")
			if e != nil || len(marks) != 2 || !marks[0].Baseline {
				t.Fatal(marks, e)
			}
		})
	}
}

func recoveryFailureModel(mode string, written chan<- struct{}) *mockmodel.Model {
	return mockmodel.New("recovery", func(ctx context.Context, r *model.Request) (*model.Response, error) {
		msg := model.NewAssistantMessage("done")
		if !hasToolMessage(r) {
			msg.ToolCalls = []model.ToolCall{{ID: "write", Type: "function", Function: model.FunctionDefinitionParam{Name: "save_file", Arguments: []byte(`{"file_name":"note","contents":"managed","overwrite":true}`)}}}
		} else {
			switch mode {
			case "crash":
				os.Exit(41)
			case "cancel":
				close(written)
				<-ctx.Done()
				return nil, ctx.Err()
			case "provider-failure":
				return nil, errors.New("provider unavailable")
			case "tool-error":
				if strings.Contains(r.Messages[len(r.Messages)-1].Content, "old string") || r.Messages[len(r.Messages)-1].ToolName == "replace_content" {
					return nil, errors.New("stop after tool failure")
				}
				msg.ToolCalls = []model.ToolCall{{ID: "bad-edit", Type: "function", Function: model.FunctionDefinitionParam{Name: "replace_content", Arguments: []byte(`{"file_name":"note","old_string":"missing","new_string":"x"}`)}}}
			}
		}
		return &model.Response{Choices: []model.Choice{{Message: msg}}}, nil
	})
}

func TestLegacySessionDoesNotInventBaseline(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	cfg := testConfig(t)
	m := mockmodel.New("legacy", mockmodel.Echo)
	rt, e := New(Options{Config: &cfg, DataDir: data, Model: m})
	if e != nil {
		t.Fatal(e)
	}
	first, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "legacy", Message: "old"})
	if e != nil {
		t.Fatal(e)
	}
	collect(t, first)
	rt.Close()
	rt, e = New(Options{Config: &cfg, DataDir: data, Model: m, TurnCheckpoints: true})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "legacy", Message: "new"})
	if e != nil {
		t.Fatal(e)
	}
	collect(t, h)
	marks, e := rt.Checkpoints(ctx, "u", "legacy")
	if e != nil || len(marks) != 1 || marks[0].Baseline || marks[0].RunID != h.ID() {
		t.Fatal(marks, e)
	}
	if e = rt.RewindSession(ctx, "u", "legacy", agent.SessionBaselineID); !errors.Is(e, agent.ErrSession) {
		t.Fatal("fabricated baseline", e)
	}
	history, e := rt.SessionHistory(ctx, "u", "legacy", 0)
	if e != nil || len(history) != 4 {
		t.Fatal(history, e)
	}
}

func TestPendingWriteAndActiveRewind(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	data := t.TempDir()
	cfg := testConfig(t)
	cfg.Workspace.Root = ws
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	if e := os.WriteFile(filepath.Join(ws, "note"), []byte("original"), 0640); e != nil {
		t.Fatal(e)
	}
	written := make(chan struct{})
	rt, e := New(Options{Config: &cfg, DataDir: data, Model: recoveryFailureModel("cancel", written), TurnCheckpoints: true})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "edit"})
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-written:
	case <-time.After(5 * time.Second):
		t.Fatal("tool did not write")
	}
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if e := rt.RewindSession(bounded, "u", "s", agent.SessionBaselineID); e == nil {
		t.Fatal("rewound active run")
	}
	h.Cancel()
	collect(t, h)
	// A crash between filesystem replacement and the status update leaves this shape.
	_, e = rt.turnDB.Exec("UPDATE grape_file_changes SET status='pending'")
	if e != nil {
		t.Fatal(e)
	}
	p, e := rt.PreviewFiles(ctx, "u", "s", agent.SessionBaselineID)
	if e != nil || len(p.Files) != 1 || !strings.Contains(p.Files[0].Conflict, "interrupted") {
		t.Fatal(p, e)
	}
	r, e := rt.RestoreFiles(ctx, "u", "s", agent.SessionBaselineID, p.Digest)
	if e == nil || r.Status != "refused" {
		t.Fatal(r, e)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "note")); string(b) != "managed" {
		t.Fatal("pending write was silently overwritten")
	}
	if e = rt.RewindSession(ctx, "u", "s", agent.SessionBaselineID); e != nil {
		t.Fatal("file conflict blocked conversation rewind", e)
	}
	if e = rt.DeleteSession(ctx, "u", "s"); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"grape_turns", "grape_file_changes", "grape_recoveries", "grape_backups"} {
		var n int
		if e := rt.turnDB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); e != nil || n != 0 {
			t.Fatal(table, n, e)
		}
	}
}

func TestCheckpointFileLimitFailsBeforeMutation(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	cfg := testConfig(t)
	cfg.Workspace.Root = ws
	rt, e := New(Options{Config: &cfg, DataDir: t.TempDir(), Model: mockmodel.New("limit", mockmodel.Echo), TurnCheckpoints: true})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "base"})
	if e != nil {
		t.Fatal(e)
	}
	collect(t, h)
	d, e := coding.OpenDirectory(ws)
	if e != nil {
		t.Fatal(e)
	}
	defer d.Close()
	limits := rt.RecoveryLimits()
	st := newRunState("too-large", "s", agent.ModeDefault)
	st.user = "u"
	err := rt.saveTracked(withState(ctx, st), d, saveIn{FileName: "large", Contents: strings.Repeat("x", limits.FileBytes+1)})
	if !errors.Is(err, agent.ErrCheckpointLimit) {
		t.Fatal(err)
	}
	if _, e := os.Stat(filepath.Join(ws, "large")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("oversized write changed workspace", e)
	}
	p, e := rt.PreviewFiles(ctx, "u", "s", h.ID())
	if e != nil || len(p.Files) != 0 {
		t.Fatal(p, e)
	}
}
