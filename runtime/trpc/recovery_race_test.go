package trpcruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/mattn/go-sqlite3"
)

func TestWriterChangesSecondFileDuringRestore(t *testing.T) {
	ctx := context.Background()
	ws := t.TempDir()
	cfg := testConfig(t)
	cfg.Workspace.Root = ws
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	rt, e := New(Options{Config: &cfg, DataDir: t.TempDir(), Model: mockmodel.New("recovery", mockmodel.Echo), TurnCheckpoints: true})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "base"})
	if e != nil {
		t.Fatal(e)
	}
	collect(t, h)
	root, e := coding.OpenDirectory(ws)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	st := newRunState("managed", "s", agent.ModeDefault)
	st.user = "u"
	for _, name := range []string{"a", "b"} {
		if e := os.WriteFile(filepath.Join(ws, name), []byte("original"), 0600); e != nil {
			t.Fatal(e)
		}
		if e := rt.saveTracked(withState(ctx, st), root, saveIn{FileName: name, Contents: "managed", Overwrite: true}); e != nil {
			t.Fatal(e)
		}
	}
	p, e := rt.PreviewFiles(ctx, "u", "s", h.ID())
	if e != nil || len(p.Files) != 2 {
		t.Fatal(p, e)
	}
	conn, e := rt.turnDB.Conn(ctx)
	if e != nil {
		t.Fatal(e)
	}
	e = conn.Raw(func(c any) error {
		return c.(*sqlite3.SQLiteConn).RegisterFunc("external_edit", func() (int, error) {
			// The first path has already committed. Replace the second path with
			// a directory just after its backup, before the final file check.
			if e := os.Rename(filepath.Join(ws, "b"), filepath.Join(ws, "saved-b")); e != nil {
				return 0, e
			}
			return 0, os.Mkdir(filepath.Join(ws, "b"), 0700)
		}, false)
	})
	conn.Close()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = rt.turnDB.Exec("CREATE TRIGGER concurrent_writer AFTER INSERT ON grape_backups WHEN NEW.path='b' BEGIN SELECT external_edit(); END"); e != nil {
		t.Fatal(e)
	}
	r, e := rt.RestoreFiles(ctx, "u", "s", h.ID(), p.Digest)
	if e == nil || r.Status != "partial" {
		t.Fatal(r, e)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a")); string(b) != "original" {
		t.Fatal("first file was not restored")
	}
	if info, e := os.Stat(filepath.Join(ws, "b")); e != nil || !info.IsDir() {
		t.Fatal("external path replacement overwritten", e)
	}
	b, e := rt.RecoveryBackup(ctx, "u", "s", r.ID, ws, "b")
	if e != nil || string(b.Data) != "managed" {
		t.Fatal(b, e)
	}
	if _, e = rt.turnDB.Exec("DROP TRIGGER concurrent_writer"); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(filepath.Join(ws, "b")); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(ws, "saved-b"), filepath.Join(ws, "b")); e != nil {
		t.Fatal(e)
	}
	p, e = rt.PreviewFiles(ctx, "u", "s", h.ID())
	if e != nil || len(p.Files) != 1 {
		t.Fatal(p, e)
	}
	r, e = rt.RestoreFiles(ctx, "u", "s", h.ID(), p.Digest)
	if e != nil || r.Status != "completed" {
		t.Fatal(r, e)
	}
}
