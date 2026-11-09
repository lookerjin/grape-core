package trpcruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/plugin"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestHookEventsTimeoutAndReadOnly(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := installHooks(t, root, "trace", `[
		{"event":"BeforeRun","command":"sh","args":["-c","echo BeforeRun >> hook.log"],"blocking":false},
		{"event":"AfterRun","command":"sh","args":["-c","echo AfterRun >> hook.log"],"blocking":false},
		{"event":"BeforeModel","command":"sh","args":["-c","echo BeforeModel >> hook.log"],"blocking":false},
		{"event":"AfterModel","command":"sh","args":["-c","echo AfterModel >> hook.log"],"blocking":false},
		{"event":"BeforeTool","command":"sh","args":["-c","printf '%s\n' \"$GRAPE_HOOK_INPUT\" >> hook.log"],"blocking":false},
		{"event":"AfterTool","command":"sh","args":["-c","echo AfterTool >> hook.log"],"blocking":false},
		{"event":"OnEvent","command":"sh","args":["-c","echo OnEvent >> hook.log"],"blocking":false},
		{"event":"BeforeTool","command":"false","blocking":false}
	]`)
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = "managed"
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	cfg.Plugins.Enabled = []string{"trace"}
	rt, err := New(Options{
		Config:        &cfg,
		Model:         mockmodel.New("hooks", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done")),
		DataDir:       t.TempDir(),
		PluginManager: mgr,
		Tools:         []tool.Tool{echoTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "hooks", Workspace: ws, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	blob := eventBlob(events)
	log, err := os.ReadFile(filepath.Join(ws, "hook.log"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(log)
	for _, want := range []string{"BeforeRun", "BeforeModel", "AfterModel", "AfterTool", "OnEvent", "AfterRun"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log missing %s:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "text") || !strings.Contains(text, "x") {
		t.Fatalf("BeforeTool payload missing tool arguments:\n%s", text)
	}
	if !strings.Contains(blob, "exited") {
		t.Fatalf("non-blocking hook failure was not an event: %s", blob)
	}

	roRoot := t.TempDir()
	roWS := filepath.Join(roRoot, "ws")
	if err := os.MkdirAll(roWS, 0o755); err != nil {
		t.Fatal(err)
	}
	roMgr := installHooks(t, roRoot, "allow", `[{"event":"BeforeTool","command":"true","blocking":true}]`)
	ro := config.Default()
	ro.Workspace.Permission = agent.PermissionReadOnly
	ro.Workspace.Root = roWS
	ro.Sandbox.Mode = "managed"
	ro.Model.Generation.Stream = boolPtr(false)
	ro.Journal.Enabled = false
	ro.Plugins.Enabled = []string{"allow"}
	roRT, err := New(Options{
		Config:        &ro,
		Model:         mockmodel.New("ro", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done")),
		DataDir:       t.TempDir(),
		PluginManager: roMgr,
		ToolSpecs:     []ToolSpec{{Tool: echoTool(), Trusted: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer roRT.Close()
	roH, err := roRT.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "ro", Workspace: roWS, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	roBlob := eventBlob(collect(t, roH))
	if strings.Contains(roBlob, "denied") || strings.Contains(roBlob, "read_only workspace") {
		t.Fatalf("read_only denied the tool because of a successful hook: %s", roBlob)
	}

	slowRoot := t.TempDir()
	slowWS := filepath.Join(slowRoot, "ws")
	if err := os.MkdirAll(slowWS, 0o755); err != nil {
		t.Fatal(err)
	}
	slowMgr := installHooks(t, slowRoot, "slow", `[{"event":"BeforeTool","command":"sleep","args":["30"],"blocking":true}]`)
	slowCfg := config.Default()
	slowCfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	slowCfg.Workspace.Root = slowWS
	slowCfg.Sandbox.Mode = "managed"
	slowCfg.Hooks.Timeout = "200ms"
	slowCfg.Model.Generation.Stream = boolPtr(false)
	slowCfg.Journal.Enabled = false
	slowCfg.Plugins.Enabled = []string{"slow"}
	slowRT, err := New(Options{
		Config:        &slowCfg,
		Model:         mockmodel.New("slow", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done")),
		DataDir:       t.TempDir(),
		PluginManager: slowMgr,
		Tools:         []tool.Tool{echoTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer slowRT.Close()
	start := time.Now()
	slowH, err := slowRT.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "slow", Workspace: slowWS, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	slowBlob := eventBlob(collect(t, slowH))
	if time.Since(start) > 5*time.Second {
		t.Fatal("cfg.Hooks.Timeout was not applied")
	}
	if !strings.Contains(slowBlob, "denied") && !strings.Contains(slowBlob, "timeout") && !strings.Contains(slowBlob, "deadline") {
		t.Fatalf("slow hook did not block the tool: %s", slowBlob)
	}
}

func TestHooksRunAgainAcrossSessions(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := installHooks(t, root, "again", `[{"event":"BeforeTool","command":"sh","args":["-c","echo hit >> hits.log"],"blocking":true}]`)
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = "managed"
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	cfg.Plugins.Enabled = []string{"again"}
	rt, err := New(Options{
		Config:        &cfg,
		Model:         mockmodel.New("again", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done")),
		DataDir:       t.TempDir(),
		PluginManager: mgr,
		Tools:         []tool.Tool{echoTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, id := range []string{"s1", "s2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			h, err := rt.Start(context.Background(), agent.RunRequest{
				UserID: "u", SessionID: id, Workspace: ws, Message: "go",
			})
			if err != nil {
				errCh <- err
				return
			}
			collect(t, h)
		}(id)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(ws, "hits.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "hit") < 2 {
		t.Fatalf("hook skipped a later session:\n%s", raw)
	}
}

func installHooks(t *testing.T, root, name, hooks string) *plugin.Manager {
	t.Helper()
	mgr, err := plugin.NewManager(filepath.Join(root, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src-"+name)
	writeBundle(t, src, name)
	if err := os.WriteFile(filepath.Join(src, "hooks", "hooks.json"), []byte(`{"hooks":`+hooks+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(src); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable(name); err != nil {
		t.Fatal(err)
	}
	return mgr
}
