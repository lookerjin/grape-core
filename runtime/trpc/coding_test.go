package trpcruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/sandbox"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestCodingFixturePreservesUnrelatedEdits(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap is not installed; managed coding run was not executed")
	}
	ws := copyFixture(t)
	gitCmd(t, ws, "init")
	gitCmd(t, ws, "config", "user.email", "test@example.com")
	gitCmd(t, ws, "config", "user.name", "test")
	gitCmd(t, ws, "add", ".")
	gitCmd(t, ws, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("dirty note stays\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("coding", mockmodel.CodingFix)
	rt := newTestRuntime(t, mdl, cfg)
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID:    "u",
		SessionID: "fix",
		Workspace: ws,
		Message:   "fix the failing test",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if events[len(events)-1].Kind != agent.EventCompleted {
		t.Fatalf("events %v last %s", kinds(events), events[len(events)-1].Payload)
	}
	body, err := os.ReadFile(filepath.Join(ws, "math.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "return a + b") || strings.Contains(string(body), "return a - b") {
		t.Fatalf("math.go not fixed:\n%s", body)
	}
	notes, err := os.ReadFile(filepath.Join(ws, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(notes) != "dirty note stays\n" {
		t.Fatalf("notes changed: %q", notes)
	}
	status, err := coding.GitStatus(context.Background(), ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "math.go") || !strings.Contains(status, "notes.txt") {
		t.Fatalf("status %q", status)
	}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go test: %v\n%s", err, out)
	}
}

func TestApprovalDenialDoesNotRunCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "sneaky")
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionDangerFullAccess
	cfg.Approvals.Required = true
	cfg.Workspace.Root = dir
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("deny", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if mockmodelRequestsHaveTool(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("stopped")}}}, nil
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-touch",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      "run_command",
						Arguments: []byte(`{"argv":["sh","-c","echo sneaky > sneaky"]}`),
					},
				}},
			},
		}}}, nil
	})
	rt, err := New(Options{
		Config:  &cfg,
		Model:   mdl,
		DataDir: t.TempDir(),
		Decider: agent.DeciderFunc(func(ctx context.Context, req agent.ApprovalRequest) (agent.Decision, error) {
			return agent.Decision{ID: req.ID, Allow: false, Reason: "no"}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "deny", Workspace: dir, Message: "write a file",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	if !hasKind(events, agent.EventPermissionRequest) {
		t.Fatalf("events %v", kinds(events))
	}
	var denied bool
	for _, evt := range events {
		if evt.Kind == agent.EventPermissionRequest && strings.Contains(string(evt.Payload), `"allowed":false`) {
			denied = true
		}
	}
	if !denied {
		t.Fatal("missing denial")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("denied command created a file")
	}
}

func mockmodelRequestsHaveTool(req *model.Request) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			return true
		}
	}
	return false
}

func hasKind(events []agent.Event, kind agent.EventKind) bool {
	for _, evt := range events {
		if evt.Kind == kind {
			return true
		}
	}
	return false
}

func copyFixture(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	src := filepath.Join(root, "testdata", "fixture")
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		in, err := os.Open(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(filepath.Join(dst, entry.Name()))
		if err != nil {
			in.Close()
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			t.Fatal(err)
		}
		in.Close()
		out.Close()
	}
	return dst
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
