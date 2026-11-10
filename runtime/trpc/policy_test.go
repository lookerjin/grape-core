package trpcruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/sandbox"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"

	localexec "trpc.group/trpc-go/trpc-agent-go/codeexecutor/local"
	trpcsandbox "trpc.group/trpc-go/trpc-agent-go/codeexecutor/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
	transfertool "trpc.group/trpc-go/trpc-agent-go/tool/transfer"
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
	built, err := rt.agentFactory(context.Background(), trpcagent.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	exec := built.(*llmagent.LLMAgent).CodeExecutor()
	if _, ok := exec.(*trpcsandbox.CodeExecutor); !ok {
		t.Fatalf("executor %T", exec)
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
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mdl,
		DataDir: t.TempDir(),
		Subagents: []SubagentSpec{{
			Name: "helper", Instruction: "answer", Model: mdl,
		}},
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

func TestSkillRunUsesTheSelectedExecutor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		permission string
		mode       string
		wantLocal  bool
		isolation  string
		marker     string
	}{
		{"managed", agent.PermissionWorkspaceWrite, sandbox.ModeManaged, false, sandbox.IsolationManaged, "sandbox-ok"},
		{"danger", agent.PermissionDangerFullAccess, sandbox.ModeManaged, true, sandbox.IsolationNone, "host-ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.wantLocal {
				requireLinuxSandbox(t)
			}
			dir := t.TempDir()
			writeSkill(t, filepath.Join(dir, "skills"))
			cfg := config.Default()
			cfg.Skills.Paths = []string{filepath.Join(dir, "skills")}
			cfg.Skills.KnowledgeOnly = boolPtr(false)
			cfg.Workspace.Permission = tc.permission
			cfg.Workspace.Root = dir
			cfg.Sandbox.Mode = tc.mode
			cfg.Journal.Enabled = false
			cfg.Model.Generation.Stream = boolPtr(false)
			marker := tc.marker
			var step int
			mdl := mockmodel.New(tc.name, func(ctx context.Context, req *model.Request) (*model.Response, error) {
				step++
				switch step {
				case 1:
					return toolCall("skill_load", `{"skill":"review"}`), nil
				case 2:
					return toolCall("skill_run", `{"skill":"review","command":"echo `+marker+`"}`), nil
				default:
					return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
				}
			})
			rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			h, err := rt.Start(context.Background(), agent.RunRequest{
				UserID: "u", SessionID: "skill-" + tc.name, Workspace: dir, Message: "run",
			})
			if err != nil {
				t.Fatal(err)
			}
			events := collect(t, h)
			blob := eventBlob(events)
			var stdout string
			for _, evt := range events {
				if evt.Kind != agent.EventToolEnd {
					continue
				}
				var payload agent.ToolPayload
				if json.Unmarshal(evt.Payload, &payload) != nil || payload.Name != "skill_run" {
					continue
				}
				var result string
				var body struct {
					Stdout string `json:"stdout"`
				}
				if json.Unmarshal(payload.Result, &result) == nil && json.Unmarshal([]byte(result), &body) == nil {
					stdout = body.Stdout
				}
			}
			if strings.TrimSpace(stdout) != marker {
				t.Fatalf("skill_run did not return %s: %s", marker, blob)
			}
			if strings.Contains(blob, "falling back to local engine") {
				t.Fatalf("implicit local fallback: %s", blob)
			}

		})
	}
}

func TestSpecSubagentDoesNotRunFencedCode(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "fence.txt")
	fence := "```python\nopen('" + marker + "','w').write('x')\n```"
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = dir
	cfg.Approvals.Required = true
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("spec-fence", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if strings.Contains(lastUserText(req), "child-fence") {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(fence)}}}, nil
		}
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("helper", `{"request":"child-fence"}`), nil
	})
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			if req.Detail == "helper" {
				return agent.Decision{ID: req.ID, Allow: true}, nil
			}
			return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
		}),
		Subagents: []SubagentSpec{{
			Name: "helper", Instruction: "child-fence", Model: mdl,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "spec-fence", Workspace: dir, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("spec subagent executed a fenced block")
	}
	if strings.Contains(blob, "Code execution") {
		t.Fatalf("spec subagent ran the code-execution processor: %s", blob)
	}
}

func TestTrustedAgentCanBypassTheProcessor(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "trusted-fence.txt")
	fence := "```python\nopen('" + marker + "','w').write('x')\n```"
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = dir
	cfg.Approvals.Required = true
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("trust-fence", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if strings.Contains(lastUserText(req), "child-fence") {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(fence)}}}, nil
		}
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		return toolCall("helper", `{"request":"child-fence"}`), nil
	})
	child := llmagent.New("helper",
		llmagent.WithModel(mdl),
		llmagent.WithInstruction("child-fence"),
		llmagent.WithCodeExecutor(localexec.New()),
		llmagent.WithGenerationConfig(model.GenerationConfig{Stream: false}),
	)
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
		}),
		TrustedAgents: []trpcagent.Agent{child},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "trust-fence", Workspace: dir, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if !strings.Contains(blob, "Code execution") {
		t.Fatalf("trusted agent did not run its code-execution processor: %s", blob)
	}
}

func TestExternalTransferIsASideEffect(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionReadOnly
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("xfer-ext", mockmodel.ToolThenText("transfer_to_agent", `{"agent_name":"helper"}`, "done"))
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Tools: []tool.Tool{transfertool.New(nil)},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "xfer-ext", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if !strings.Contains(blob, "read_only") {
		t.Fatalf("external transfer was allowed: %s", blob)
	}
}

func TestApprovalSeesArgvAndPath(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = dir
	cfg.Approvals.Required = true
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	var got []agent.ApprovalRequest
	var step int
	mdl := mockmodel.New("argv", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		step++
		switch step {
		case 1:
			return toolCall("run_command", `{"argv":["echo","hi"]}`), nil
		case 2:
			return toolCall("save_file", `{"file_name":"note.txt","contents":"hi"}`), nil
		default:
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
	})
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			got = append(got, req)
			return agent.Decision{ID: req.ID, Allow: true}, nil
		}),
		ToolSpecs: []ToolSpec{{
			Tool: functionToolNamed("custom_cmd"),
			Kind: "command",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "argv", Workspace: dir, Message: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if len(got) < 2 {
		t.Fatalf("requests %#v", got)
	}
	if got[0].Kind != "command" || got[0].Detail != "echo hi" || got[0].Path != "" {
		t.Fatalf("command request %#v", got[0])
	}
	if got[1].Kind != "write" || got[1].Detail != "note.txt" || got[1].Path != "note.txt" {
		t.Fatalf("write request %#v", got[1])
	}
}

func TestToolSpecKindSurvives(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionReadOnly
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	mdl := mockmodel.New("kind", mockmodel.ToolThenText("custom_cmd", `{"argv":["true"]}`, "done"))
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		ToolSpecs: []ToolSpec{{Tool: functionToolNamed("custom_cmd"), Kind: "command"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "kind", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if !strings.Contains(blob, `"kind":"command"`) || !strings.Contains(blob, `"detail":"true"`) {
		t.Fatalf("kind was dropped: %s", blob)
	}
	if !strings.Contains(blob, "read_only") {
		t.Fatalf("command was not denied: %s", blob)
	}
}

func TestTrustedWrapperIsStableUnderConcurrency(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mockmodel.New("race", mockmodel.ToolThenText("echo", `{"text":"x"}`, "done")),
		DataDir: t.TempDir(),
		ToolSpecs: []ToolSpec{{
			Tool:    echoTool(),
			Trusted: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var wg sync.WaitGroup
	errCh := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h, err := rt.Start(context.Background(), agent.RunRequest{
				UserID: "u", SessionID: fmt.Sprintf("race-%d", i), Message: "go",
			})
			if err != nil {
				errCh <- err
				return
			}
			collect(t, h)
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestSliceToolDoesNotPanic(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	item := sliceTool{parts: []string{"a"}}
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mockmodel.New("slice", mockmodel.ToolThenText("slice", `{}`, "done")),
		DataDir: t.TempDir(),
		Tools:   []tool.Tool{item},
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "slice", Message: "go"})
	if err != nil {
		rt.Close()
		t.Fatal(err)
	}
	_ = collect(t, h)
	rt.Close()

	rt, err = New(Options{
		Config:  &cfg,
		Model:   mockmodel.New("slice-trust", mockmodel.ToolThenText("slice", `{}`, "done")),
		DataDir: t.TempDir(),
		ToolSpecs: []ToolSpec{{
			Tool:    sliceTool{parts: []string{"b", "c"}},
			Trusted: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err = rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "slice-trust", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
}

type sliceTool struct {
	parts []string
}

func (s sliceTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "slice", Description: strings.Join(s.parts, ",")}
}

func (s sliceTool) Call(context.Context, []byte) (any, error) {
	return "ok", nil
}

func functionToolNamed(name string) tool.Tool {
	return functionTool{name: name}
}

type functionTool struct{ name string }

func (f functionTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: f.name}
}

func (f functionTool) Call(context.Context, []byte) (any, error) {
	return "ok", nil
}

func TestSkillRunDenySeesTheCommand(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, filepath.Join(dir, "skills"))
	marker := filepath.Join(t.TempDir(), "pwned")
	command := "touch " + marker
	cfg := config.Default()
	cfg.Skills.Paths = []string{filepath.Join(dir, "skills")}
	cfg.Skills.KnowledgeOnly = boolPtr(false)
	cfg.Workspace.Permission = agent.PermissionDangerFullAccess
	cfg.Workspace.Root = dir
	cfg.Sandbox.Mode = sandbox.ModeDangerFullAccess
	cfg.Approvals.Required = true
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	var step int
	var got agent.ApprovalRequest
	mdl := mockmodel.New("skill-deny", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		step++
		switch step {
		case 1:
			return toolCall("skill_load", `{"skill":"review"}`), nil
		case 2:
			return toolCall("skill_run", `{"skill":"review","command":`+fmt.Sprintf("%q", command)+`}`), nil
		default:
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
	})
	rt, err := New(Options{
		Config: &cfg, Model: mdl, DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			got = req
			return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "skill-deny", Workspace: dir, Message: "run",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = collect(t, h)
	if !strings.Contains(got.Detail, "review") || !strings.Contains(got.Detail, command) {
		t.Fatalf("decider saw %#v", got)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("denied skill_run created %s (%v)", marker, err)
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
