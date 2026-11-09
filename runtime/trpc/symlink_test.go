package trpcruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestWorkspaceFileToolsRejectSymlinks(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(outside, "dir")
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "inside.txt"), []byte("DIR-SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(ws, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "missing-target"), filepath.Join(ws, "dangling")); err != nil {
		t.Fatal(err)
	}

	calls := []struct {
		name string
		args string
	}{
		{"read_file", `{"file_name":"link.txt"}`},
		{"save_file", `{"file_name":"link.txt","contents":"PWNED","overwrite":true}`},
		{"replace_content", `{"file_name":"link.txt","old_string":"TOP","new_string":"NO","num_replacements":1}`},
		{"list_file", `{"path":"linkdir"}`},
		{"search_file", `{"path":"linkdir","pattern":"*.txt"}`},
		{"search_content", `{"path":".","content_pattern":"zzz-not-in-workspace"}`},
		{"read_multiple_files", `{"patterns":["linkdir/*","link.txt"]}`},
		{"read_file", `{"file_name":"dangling"}`},
		{"save_file", `{"file_name":"linkdir/new.txt","contents":"PWNED","overwrite":true}`},
		{"read_file", `{"file_name":"ok.txt"}`},
		{"replace_content", `{"file_name":"ok.txt","old_string":"hello","new_string":"hello!","num_replacements":1}`},
	}
	var script []string
	for _, call := range calls {
		script = append(script, call.name+"\n"+call.args)
	}
	events := runScript(t, ws, agent.PermissionWorkspaceWrite, sandbox.ModeDangerFullAccess, script)
	blob := eventBlob(events)
	if strings.Contains(blob, "TOP-SECRET") || strings.Contains(blob, "DIR-SECRET") {
		t.Fatalf("tool output leaked outside content:\n%s", blob)
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "TOP-SECRET" {
		t.Fatalf("outside file changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("save_file created a file through a directory symlink")
	}
	edited, err := os.ReadFile(filepath.Join(ws, "ok.txt"))
	if err != nil || string(edited) != "hello!" {
		t.Fatalf("in-workspace edit failed: %q %v", edited, err)
	}
}

func TestSandboxSymlinkThenHostFileTool(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Fatal("bubblewrap is required")
	}
	secretDir := t.TempDir()
	secret := filepath.Join(secretDir, "id_rsa")
	if err := os.WriteFile(secret, []byte("PRIVATE-KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	script := []string{
		"run_command\n" + `{"argv":["ln","-s",` + quote(secret) + `,"leak"]}`,
		`read_file` + "\n" + `{"file_name":"leak"}`,
		`save_file` + "\n" + `{"file_name":"leak","contents":"PWNED","overwrite":true}`,
	}
	events := runScript(t, ws, agent.PermissionWorkspaceWrite, sandbox.ModeManaged, script)
	blob := eventBlob(events)
	if strings.Contains(blob, "PRIVATE-KEY") {
		t.Fatalf("read_file followed the sandbox-planted symlink:\n%s", blob)
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "PRIVATE-KEY" {
		t.Fatalf("save_file wrote through the symlink: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, "leak")); err != nil {
		t.Fatal("expected the symlink to exist inside the workspace")
	}
}

func runScript(t *testing.T, ws, permission, mode string, steps []string) []agent.Event {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace.Permission = permission
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = mode
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	step := 0
	mdl := mockmodel.New("symlink", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if step >= len(steps) || hasToolMessage(req) && step >= len(steps) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		if hasToolMessage(req) && step < len(steps) {
			// advance after each tool result
		}
		if step >= len(steps) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		parts := strings.SplitN(steps[step], "\n", 2)
		id := step
		step++
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:       "call-" + parts[0] + "-" + itoa(id),
					Type:     "function",
					Function: model.FunctionDefinitionParam{Name: parts[0], Arguments: []byte(parts[1])},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "sym", Workspace: ws, Message: "inspect",
	})
	if err != nil {
		t.Fatal(err)
	}
	return collect(t, h)
}

func eventBlob(events []agent.Event) string {
	var b strings.Builder
	for _, evt := range events {
		b.Write(evt.Payload)
		b.WriteByte('\n')
	}
	return b.String()
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
