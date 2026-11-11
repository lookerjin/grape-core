package trpcruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestFileRecoveryPartialConflictAndRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	workspace := t.TempDir()
	cfg := testConfig(t)
	cfg.Workspace.Root = workspace
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	write := func(path, text string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(workspace, path), []byte(text), 0640); e != nil {
			t.Fatal(e)
		}
	}
	read := func(path string) string {
		t.Helper()
		d, e := os.ReadFile(filepath.Join(workspace, path))
		if e != nil {
			t.Fatal(e)
		}
		return string(d)
	}
	write("a", "before-a")
	write("b", "before-b")
	m := mockmodel.New("write", func(ctx context.Context, r *model.Request) (*model.Response, error) {
		last := r.Messages[len(r.Messages)-1]
		msg := model.NewAssistantMessage("done")
		if last.Role == model.RoleUser && (last.Content == "a" || last.Content == "b") {
			msg.ToolCalls = []model.ToolCall{{ID: "write-" + last.Content, Type: "function", Function: model.FunctionDefinitionParam{Name: "save_file", Arguments: []byte(`{"file_name":"` + last.Content + `","contents":"owned","overwrite":true}`)}}}
		}
		return &model.Response{Choices: []model.Choice{{Message: msg}}}, nil
	})
	open := func() *Runtime {
		rt, e := New(Options{Config: &cfg, DataDir: dir, Model: m, TurnCheckpoints: true})
		if e != nil {
			t.Fatal(e)
		}
		return rt
	}
	rt := open()
	defer func() { rt.Close() }()
	turn := func(msg string) string {
		t.Helper()
		h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: msg})
		if e != nil {
			t.Fatal(e)
		}
		collect(t, h)
		if h.Status().Status != "completed" {
			t.Fatal(h.Status())
		}
		return h.ID()
	}
	target := turn("base")
	turn("a")
	turn("b")
	if read("a") != "owned" || read("b") != "owned" {
		t.Fatal("tool writes missing")
	}
	rt.Close()
	rt = open()
	preview := func() agent.FileRestorePlan {
		p, e := rt.PreviewFiles(ctx, "u", "s", target)
		if e != nil {
			t.Fatal(e)
		}
		return p
	}
	p := preview()
	if len(p.Files) != 2 {
		t.Fatal(p)
	}
	write("b", "from editor")
	r, e := rt.RestoreFiles(ctx, "u", "s", target, p.Digest)
	if e == nil || r.Status != "refused" || read("a") != "owned" || read("b") != "from editor" {
		t.Fatal(r, e)
	}
	p = preview()
	if !strings.Contains(p.Files[1].Conflict, "differs") {
		t.Fatal(p)
	}
	write("b", "owned")
	if e = rt.RewindSession(ctx, "u", "s", target); e != nil {
		t.Fatal(e)
	}
	// Fail the second backup to prove that history, completed files and retries are independent.
	_, e = rt.turnDB.Exec("CREATE TRIGGER fault BEFORE INSERT ON grape_backups WHEN NEW.path='b' BEGIN SELECT RAISE(ABORT,'fault'); END")
	if e != nil {
		t.Fatal(e)
	}
	p = preview()
	r, e = rt.RestoreFiles(ctx, "u", "s", target, p.Digest)
	if e == nil || r.Status != "partial" || read("a") != "before-a" || read("b") != "owned" {
		t.Fatal(r, e)
	}
	image, e := rt.RecoveryBackup(ctx, "u", "s", r.ID, workspace, "a")
	if e != nil || string(image.Data) != "owned" {
		t.Fatal(image, e)
	}
	if _, e = rt.RecoveryBackup(ctx, "another", "s", r.ID, workspace, "a"); e == nil {
		t.Fatal("backup scope")
	}
	if _, e = rt.turnDB.Exec("DROP TRIGGER fault"); e != nil {
		t.Fatal(e)
	}
	rt.Close()
	rt = open()
	p = preview()
	if len(p.Files) != 1 {
		t.Fatal(p)
	}
	if _, e := rt.turnDB.Exec("CREATE TRIGGER after_file BEFORE UPDATE OF status ON grape_file_changes WHEN NEW.status='restored' BEGIN SELECT RAISE(ABORT,'fault after rename'); END"); e != nil {
		t.Fatal(e)
	}
	r, e = rt.RestoreFiles(ctx, "u", "s", target, p.Digest)
	if e == nil || r.Status != "partial" || read("b") != "before-b" {
		t.Fatal(r, e)
	}
	backups, e := rt.RecoveryBackups(ctx, "u", "s", r.ID)
	if e != nil || len(backups) != 1 || backups[0].Status != "prepared" {
		t.Fatal(backups, e)
	}
	if _, e := rt.turnDB.Exec("DROP TRIGGER after_file"); e != nil {
		t.Fatal(e)
	}
	rt.Close()
	rt = open()
	p = preview()
	if len(p.Files) != 1 || p.Files[0].Action != "confirm_previous_restore" || p.Files[0].Conflict != "" {
		t.Fatal(p)
	}
	r, e = rt.RestoreFiles(ctx, "u", "s", target, p.Digest)
	if e != nil || r.Status != "completed" || read("b") != "before-b" {
		t.Fatal(r, e)
	}
	log, e := rt.Recoveries(ctx, "u", "s")
	if e != nil || len(log) != 5 || log[1].Kind != "conversation" || log[1].Status != "completed" || log[2].Status != "partial" {
		t.Fatal(log, e)
	}
}
