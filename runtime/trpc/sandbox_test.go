package trpcruntime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestWindowsPlatformDoesNotExecuteManagedCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	execer := newCommandExecutor(cfg, t.TempDir(), "", "windows", nil)
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
	execer := newCommandExecutor(cfg, t.TempDir(), "", "linux", nil)
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

func TestManagedSandboxDoesNotHideHostReads(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal("bubblewrap is required to verify host-read behavior")
	}
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	const secretText = "super-secret-value"
	if err := os.WriteFile(secret, []byte(secretText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Sandbox.Network = "restricted"
	execer := newCommandExecutor(cfg, filepath.Join(t.TempDir(), "sb"), "", "linux", nil)
	res, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: dir,
		Argv:      []string{"cat", secret},
		Timeout:   20 * time.Second,
	})
	if err != nil {
		t.Fatalf("cat returned %v stderr %s", err, res.Stderr)
	}
	if !strings.Contains(res.Stdout, secretText) {
		t.Fatalf("expected the upstream profile to allow the host read, stdout %q stderr %s", res.Stdout, res.Stderr)
	}
	caps, err := execer.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.FilesystemIsolation {
		t.Fatal("FilesystemIsolation is true even though the secret was readable")
	}
	if caps.ManagedIsolation != true || !strings.Contains(caps.Reason, "--ro-bind") {
		t.Fatalf("caps %#v", caps)
	}
}

func TestManagedSandboxDeniesHostListener(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal("bubblewrap is required to verify network isolation")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required to dial the test listener")
	}
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
			accepted <- struct{}{}
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Sandbox.Network = "restricted"
	execer := newCommandExecutor(cfg, filepath.Join(t.TempDir(), "sb"), "", "linux", nil)
	script := fmt.Sprintf("python3 -c 'import socket; socket.create_connection((\"127.0.0.1\", %d), 2)'", port)
	res, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: dir,
		Argv:      []string{"sh", "-c", script},
		Timeout:   15 * time.Second,
	})
	if err == nil && res.ExitCode == 0 {
		t.Fatalf("restricted sandbox connected, stdout %q stderr %s", res.Stdout, res.Stderr)
	}
	select {
	case <-accepted:
		t.Fatal("host listener accepted a connection from the sandbox")
	case <-time.After(500 * time.Millisecond):
	}
	caps, err := execer.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.NetworkIsolation {
		t.Fatalf("network isolation flag false: %#v", caps)
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
	execer := newCommandExecutor(cfg, filepath.Join(t.TempDir(), "sb"), "", "linux", nil)
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

func TestManagedSandboxMasksSensitivePathOnly(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal("bubblewrap is required to verify sensitive path masks")
	}
	home := t.TempDir()
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "id_rsa"), []byte("PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(outside, []byte("public-note"), 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Sandbox.Network = "restricted"
	cfg.Sandbox.DenyReadMode = "override"
	cfg.Sandbox.DenyRead = []string{sshDir}
	execer := newCommandExecutor(cfg, filepath.Join(t.TempDir(), "sb"), "", "linux", nil)
	denied, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: work,
		Argv:      []string{"cat", filepath.Join(sshDir, "id_rsa")},
		Timeout:   20 * time.Second,
	})
	if strings.Contains(denied.Stdout, "PRIVATE-KEY") {
		t.Fatalf("sensitive file was readable: stdout %q stderr %s err %v", denied.Stdout, denied.Stderr, err)
	}
	if err == nil && denied.ExitCode == 0 {
		t.Fatalf("cat of masked path succeeded: %+v", denied)
	}
	open, err := execer.Execute(context.Background(), sandbox.Request{
		Workspace: work,
		Argv:      []string{"cat", outside},
		Timeout:   20 * time.Second,
	})
	if err != nil || !strings.Contains(open.Stdout, "public-note") {
		t.Fatalf("ordinary outside file was not readable: %+v err %v", open, err)
	}
	caps, err := execer.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.FilesystemIsolation {
		t.Fatal("FilesystemIsolation must stay false")
	}
	if len(caps.SensitivePathsMasked) != 1 || caps.SensitivePathsMasked[0] != sshDir {
		t.Fatalf("masked %#v", caps.SensitivePathsMasked)
	}
}

func TestDangerCancelKillsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancellation is unverified on Windows")
	}
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionDangerFullAccess
	execer := newCommandExecutor(cfg, t.TempDir(), "", "linux", nil)
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

func TestManagedCommandDoesNotInheritHostSecrets(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap verification is Linux")
	}
	t.Setenv("FAKE_API_KEY", "host-secret-value")
	ws := t.TempDir()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("env", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("run_command", `{"argv":["printenv","FAKE_API_KEY"]}`), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "env", Workspace: ws, Message: "print",
	})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if strings.Contains(blob, "host-secret-value") {
		t.Fatalf("managed command inherited FAKE_API_KEY: %s", blob)
	}
	if !strings.Contains(blob, "managed") {
		t.Fatalf("command did not run in the managed sandbox: %s", blob)
	}
}

func shellSingle(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}
