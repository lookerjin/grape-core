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
	"github.com/lookerjin/grape-core/sandbox"
)

func TestWindowsPlatformDoesNotExecuteManagedCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	execer := newCommandExecutor(cfg, t.TempDir(), "windows", nil)
	_, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: dir,
		Argv:      []string{"sh", "-c", "echo ran > " + marker},
	})
	if err == nil || !errors.Is(err, agent.ErrSandboxUnavailable) {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("command ran despite windows managed sandbox being unavailable")
	}
	caps, err := execer.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.ManagedIsolation || caps.Platform != "windows" || caps.Reason == "" {
		t.Fatalf("caps %#v", caps)
	}
}

func TestReadOnlyRejectsCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionReadOnly
	execer := newCommandExecutor(cfg, t.TempDir(), "linux", nil)
	_, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: dir,
		Argv:      []string{"sh", "-c", "echo ran > ran"},
	})
	if err == nil || !errors.Is(err, agent.ErrPermissionDenied) {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatal("read_only command ran")
	}
}

func TestManagedSandboxDeniesOutsideWrite(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed; managed sandbox was not executed")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.txt")
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	execer := newCommandExecutor(cfg, filepath.Join(t.TempDir(), "sb"), "linux", nil)
	res, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: dir,
		Argv:      []string{"sh", "-c", "echo ok > inside.txt; echo bad > " + shellSingle(outside)},
		Timeout:   30 * time.Second,
	})
	if err != nil && res.ExitCode == 0 {
		t.Fatal(err)
	}
	if res.Isolation != sandbox.IsolationManaged {
		t.Fatalf("isolation %q stderr %s err %v", res.Isolation, res.Stderr, err)
	}
	inside, err := os.ReadFile(filepath.Join(dir, "inside.txt"))
	if err != nil || strings.TrimSpace(string(inside)) != "ok" {
		t.Fatalf("inside %q err %v stdout %s stderr %s", inside, err, res.Stdout, res.Stderr)
	}
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatal("sandbox wrote outside the workspace")
	}
}

func TestDangerCancelKillsProcess(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionDangerFullAccess
	execer := newCommandExecutor(cfg, t.TempDir(), "linux", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
		close(done)
	}()
	_, err := execer.Execute(ctx, sandbox.Request{
		Workspace: dir,
		Argv:      []string{"sleep", "30"},
		Timeout:   time.Minute,
	})
	<-done
	if err == nil || !errors.Is(err, agent.ErrRunCancelled) {
		t.Fatalf("err=%v", err)
	}
	time.Sleep(100 * time.Millisecond)
	out, _ := exec.Command("pgrep", "-a", "sleep").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "sleep 30") {
			t.Fatalf("sleep survived cancel: %s", out)
		}
	}
}

func shellSingle(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
