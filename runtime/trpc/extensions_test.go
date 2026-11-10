package trpcruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/plugin"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestPluginSnapshotAssemblesSkillAndKeepsOldRuntime(t *testing.T) {
	root := t.TempDir()
	mgr, err := plugin.NewManager(filepath.Join(root, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")
	writeBundle(t, src, "bundle")
	if _, err := mgr.InstallLocal(src); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("bundle"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	cfg.Plugins.Enabled = []string{"bundle"}
	mdl := mockmodel.New("skills", mockmodel.Echo)
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), PluginManager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if len(rt.Plugins().Components) == 0 {
		t.Fatal("no components")
	}
	if len(rt.MCPServers()) != 1 || rt.MCPServers()[0].Name != "demo" {
		t.Fatalf("mcp %#v", rt.MCPServers())
	}
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "s", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	blob := ""
	if reqs := mdl.Requests(); len(reqs) > 0 {
		blob = messagesText(reqs[0])
	}
	if !strings.Contains(blob, "review") {
		t.Fatalf("skill was not visible to the model: %s", blob)
	}
	if err := mgr.Disable("bundle"); err != nil {
		t.Fatal(err)
	}
	if len(rt.Plugins().Plugins) != 1 || !rt.Plugins().Plugins[0].Enabled {
		t.Fatal("existing runtime changed its plugin snapshot")
	}
}

func TestHookCannotBypassExecutor(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr, err := plugin.NewManager(filepath.Join(root, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")
	writeBundle(t, src, "guard")
	hookPath := filepath.Join(src, "hooks", "hooks.json")
	body := `{"hooks":[{"event":"BeforeTool","command":"sh","args":["-c","echo pwned > hooked.txt"],"blocking":true}]}`
	if err := os.WriteFile(hookPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(src); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("guard"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionReadOnly
	cfg.Workspace.Root = ws
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	cfg.Plugins.Enabled = []string{"guard"}
	cfg.Hooks.Allow = []string{"guard/hooks"}
	mdl := mockmodel.New("hook", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done"))
	rt, err := New(Options{
		Config:        &cfg,
		Model:         mdl,
		DataDir:       t.TempDir(),
		PluginManager: mgr,
		Tools:         []tool.Tool{echoTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "hook", Workspace: ws, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if events[len(events)-1].Kind != agent.EventCompleted && events[len(events)-1].Kind != agent.EventFailed {
		t.Fatalf("last %s", events[len(events)-1].Kind)
	}
	if _, err := os.Stat(filepath.Join(ws, "hooked.txt")); !os.IsNotExist(err) {
		t.Fatal("hook wrote a file without permission")
	}
}

func writeBundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(`{"name":"`+name+`","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: review\ndescription: Review code carefully\n---\nRead the diff.\n"
	if err := os.WriteFile(filepath.Join(dir, "skills", "review", "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), []byte(`{"hooks":[{"event":"BeforeTool","command":"echo","args":["ok"],"blocking":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "rules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules", "style.md"), []byte("Prefer small patches.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"demo":{"command":"echo","args":["mcp"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}
