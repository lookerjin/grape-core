package trpcruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/sandbox"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	trpcsandbox "trpc.group/trpc-go/trpc-agent-go/codeexecutor/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
)

func TestDeciderAllowRunsTheToolOnce(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = true
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	var asked int
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mockmodel.New("allow-once", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done")),
		DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			asked++
			return agent.Decision{ID: req.ID, Allow: true}, nil
		}),
		Tools: []tool.Tool{echoTool()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "allow-once", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if asked != 1 {
		t.Fatalf("decider called %d times: %s", asked, blob)
	}
	if !strings.Contains(blob, "x") {
		t.Fatalf("allowed tool did not run: %s", blob)
	}
}

func TestRawAgentToolRequiresTrusted(t *testing.T) {
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("raw", mockmodel.Echo)
	child := llmagent.New("helper",
		llmagent.WithModel(mdl),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
	)
	raw := agenttool.NewTool(child)
	if _, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), Tools: []tool.Tool{raw}}); err == nil {
		t.Fatal("raw agent tool in Tools was accepted")
	}
	if _, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir(), ToolSpecs: []ToolSpec{{Tool: raw}}}); err == nil {
		t.Fatal("raw agent tool without Trusted was accepted")
	}
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		ToolSpecs: []ToolSpec{{Tool: raw, Trusted: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.Close()
}

func TestUnauthorizedPluginHookNeverStarts(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := installHooks(t, root, "sneak", `[{"event":"BeforeRun","command":"sh","args":["-c","echo pwned > hooked.txt"],"blocking":true}]`)
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = sandbox.ModeDangerFullAccess
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Plugins.Enabled = []string{"sneak"}
	rt, err := New(Options{
		Config:        &cfg,
		Model:         mockmodel.New("sneak", mockmodel.Echo),
		DataDir:       t.TempDir(),
		PluginManager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "sneak", Workspace: ws, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if _, err := os.Stat(filepath.Join(ws, "hooked.txt")); !os.IsNotExist(err) {
		t.Fatal("plugin hook ran without hooks.allow")
	}
}

func TestAllowedHookStartsUnderHostMode(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	mgr := installHooks(t, root, "granted", `[{"event":"BeforeRun","command":"sh","args":["-c","echo pwned > hooked.txt"],"blocking":true}]`)
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = sandbox.ModeDangerFullAccess
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Plugins.Enabled = []string{"granted"}
	cfg.Hooks.Allow = []string{"granted/hooks"}
	rt, err := New(Options{
		Config:        &cfg,
		Model:         mockmodel.New("granted", mockmodel.Echo),
		DataDir:       t.TempDir(),
		PluginManager: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "granted", Workspace: ws, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	raw, err := os.ReadFile(filepath.Join(ws, "hooked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "pwned") {
		t.Fatalf("allowed hook did not run: %s", raw)
	}
}

func TestFencedCodeIsNotExecuted(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "skills"))
	marker := filepath.Join(dir, "pwned.txt")
	cfg := config.Default()
	cfg.Skills.Paths = []string{filepath.Join(dir, "skills")}
	cfg.Skills.KnowledgeOnly = boolPtr(false)
	cfg.Workspace.Permission = agent.PermissionDangerFullAccess
	cfg.Workspace.Root = dir
	cfg.Sandbox.Mode = sandbox.ModeDangerFullAccess
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	fence := "```python\nopen('pwned.txt','w').write('x')\n```"
	mdl := mockmodel.New("fence", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(fence)}}}, nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "fence", Workspace: dir, Message: "code",
	})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("fenced code ran: %s", blob)
	}
	if strings.Contains(blob, "Code execution result") {
		t.Fatalf("code execution processor ran: %s", blob)
	}
	rt.mu.Lock()
	exec := rt.boundExec
	isolation := rt.boundIsolation
	rt.mu.Unlock()
	if _, ok := exec.(*hostCodeExecutor); !ok {
		t.Fatalf("executor %T isolation %s", exec, isolation)
	}
	if isolation != sandbox.IsolationNone {
		t.Fatalf("danger_full_access isolation %q", isolation)
	}
	if strings.Contains(fmt.Sprintf("%T", exec), "local.CodeExecutor") {
		t.Fatal("local executor was selected")
	}
}

func TestKnowledgeOnlyFalseUsesSandboxExecutor(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "skills"))
	cfg := config.Default()
	cfg.Skills.Paths = []string{filepath.Join(dir, "skills")}
	cfg.Skills.KnowledgeOnly = boolPtr(false)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = dir
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mockmodel.New("skill-exec", mockmodel.Echo),
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "skill-exec", Workspace: dir, Message: "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	rt.mu.Lock()
	exec := rt.boundExec
	isolation := rt.boundIsolation
	rt.mu.Unlock()
	if _, ok := exec.(*trpcsandbox.CodeExecutor); !ok {
		t.Fatalf("executor %T isolation %s", exec, isolation)
	}
	if isolation != sandbox.IsolationManaged {
		t.Fatalf("isolation %q", isolation)
	}
	if strings.Contains(fmt.Sprintf("%T", exec), "local.CodeExecutor") {
		t.Fatal("local executor was selected")
	}
}

func TestWindowsManagedSkillExecutorFailsClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	execer := newCommandExecutor(cfg, t.TempDir(), t.TempDir(), "windows", nil)
	_, isolation, err := execer.skillCodeExecutor(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "windows") {
		t.Fatalf("err=%v isolation=%s", err, isolation)
	}
}

func TestStaticSubagentIsNotARawAgentTool(t *testing.T) {
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("static-sub", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if strings.Contains(lastUserText(req), "child-task") {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("child-ok")}}}, nil
		}
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("parent-done")}}}, nil
		}
		return toolCall("helper", `{"request":"child-task"}`), nil
	})
	child := llmagent.New("helper",
		llmagent.WithModel(mdl),
		llmagent.WithInstruction("answer"),
		llmagent.WithEnableCodeExecutionResponseProcessor(false),
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
	)
	rt, err := New(Options{
		Config:    &cfg,
		Model:     mdl,
		DataDir:   t.TempDir(),
		SubAgents: []trpcagent.Agent{child},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "static-sub", Message: "delegate"})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if !strings.Contains(blob, "child-ok") {
		t.Fatalf("static subagent did not run under the inherited policy: %s", blob)
	}
}

func writeSkill(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: review\ndescription: Review code\n---\nRead the diff.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
