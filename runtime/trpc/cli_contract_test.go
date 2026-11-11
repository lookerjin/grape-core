package trpcruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/plugin"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

func TestPerRunPluginWorkspace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	extra := filepath.Join(root, "extra")
	if err := os.Mkdir(extra, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := quietConfig(t)
	cfg.Workspace.Root = t.TempDir()
	cfg.Workspace.ExtraRoots = []string{extra}
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	cfg.Artifacts.Enabled = true
	mgr, err := plugin.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir()
	for path, body := range map[string]string{".claude-plugin/plugin.json": `{"name":"fixture"}`, "agents/editor.md": "---\ntools: [read_file, save_file]\n---\nEdit requested file."} {
		path = filepath.Join(src, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mgr.InstallLocal(src); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("fixture"); err != nil {
		t.Fatal(err)
	}
	mdl := mockmodel.New("fixture", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		_, write := req.Tools["save_file"]
		if (lastUserText(req) == "write") != write {
			return nil, fmt.Errorf("wrong write grant: %v", write)
		}
		if _, ok := req.Tools["read_file"]; !ok {
			return nil, errors.New("missing declared read grant")
		}
		if len(req.Tools) > 2 {
			return nil, errors.New("undeclared plugin tools")
		}
		if req.Messages[len(req.Messages)-1].Role == model.RoleUser {
			args, _ := json.Marshal(saveIn{Root: extra, FileName: "result", Contents: "approved"})
			return mockmodel.ToolCall("save_file", string(args)), nil
		}
		return mockmodel.Echo(ctx, req)
	})
	var approvals []agent.ApprovalRequest
	opts := Options{Config: &cfg, PluginManager: mgr, Model: mdl, DataDir: t.TempDir(), Decider: agent.DeciderFunc(func(ctx context.Context, q agent.ApprovalRequest) (agent.Decision, error) {
		approvals = append(approvals, q)
		return agent.Decision{Allow: true}, nil
	})}
	if bad, err := New(opts); err == nil {
		bad.Close()
		t.Fatal("plugin acquired undeclared grant")
	}
	cfg.Plugins.AllowBuiltinTools = map[string][]string{"fixture/editor": {"read_file", "save_file"}}
	rt, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	moved := filepath.Join(root, "original")
	if err = os.Rename(extra, moved); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, extra); err != nil {
		t.Fatal(err)
	}
	q := agent.RunRequest{UserID: "u", SessionID: "s", AgentID: "editor", Permission: agent.PermissionReadOnly, Message: "read"}
	h, err := rt.Start(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	collect(t, h)
	if _, err = os.Stat(filepath.Join(moved, "result")); !os.IsNotExist(err) || len(approvals) != 0 {
		t.Fatalf("read-only write or approval: %v %v", err, approvals)
	}
	q.Permission = agent.PermissionWorkspaceWrite
	q.RequireApproval = true
	q.Message = "write"
	h, err = rt.Start(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	evs := collect(t, h)
	got, err := os.ReadFile(filepath.Join(moved, "result"))
	if err != nil || string(got) != "approved" {
		t.Fatalf("write failed: %v %s %#v", err, got, h.Status())
	}
	if _, err = os.Stat(filepath.Join(outside, "result")); !os.IsNotExist(err) {
		t.Fatalf("replaced grant followed: %v", err)
	}
	if len(approvals) != 1 || approvals[0].AgentID != "editor" || approvals[0].InvocationID == "" || approvals[0].Path != filepath.Join(extra, "result") {
		t.Fatal(approvals)
	}
	found := false
	for _, ev := range evs {
		if ev.Kind == agent.EventPermissionRequest {
			var p agent.PermissionPayload
			if err = json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			found = p.ID == approvals[0].ID && p.InvocationID == approvals[0].InvocationID
		}
	}
	if !found {
		t.Fatal("approval decision could not be correlated")
	}
	store, err := rt.Artifacts("u", "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Save(ctx, "result.txt", "text/plain", got); err != nil {
		t.Fatal(err)
	}
	q.Permission = agent.PermissionDangerFullAccess
	if _, err = rt.Start(ctx, q); err == nil {
		t.Fatal("raised runtime ceiling")
	}
}

func TestChildInvocationCancellation(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = true
	started := make(chan agent.ApprovalRequest, 1)
	decider := agent.DeciderFunc(func(ctx context.Context, q agent.ApprovalRequest) (agent.Decision, error) {
		if q.AgentID == "grape" {
			return agent.Decision{Allow: true}, nil
		}
		started <- q
		<-ctx.Done()
		return agent.Decision{}, ctx.Err()
	})
	write := function.NewFunctionTool(func(context.Context, struct{}) (string, error) {
		t.Error("cancelled child tool executed")
		return "", nil
	}, function.WithName("write_note"))
	child := mockmodel.New("child", func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		return mockmodel.ToolCall("write_note", "{}"), nil
	})
	parent := mockmodel.New("parent", func(ctx context.Context, r *model.Request) (*model.Response, error) {
		if hasToolMessage(r) {
			return mockmodel.Echo(ctx, r)
		}
		return mockmodel.ToolCall("helper", `{"request":"go"}`), nil
	})
	rt, err := New(Options{Config: &cfg, Model: parent, DataDir: t.TempDir(), Decider: decider, Subagents: []SubagentSpec{{Name: "helper", Model: child, ToolSpecs: []ToolSpec{{Tool: write, Kind: "write"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "delegate"})
	if err != nil {
		t.Fatal(err)
	}
	var approval agent.ApprovalRequest
	select {
	case approval = <-started:
	case <-ctx.Done():
		t.Fatal("child never started")
	}
	ins := h.(agent.RunInspector)
	calls := ins.Subagents()
	if len(calls) != 1 || calls[0].Status != "running" || calls[0].ParentInvocationID == "" || calls[0].Model != "child" || calls[0].InvocationID != approval.InvocationID || calls[0].AgentID != approval.AgentID || calls[0].ToolCallID == "" {
		t.Fatalf("running children: %+v", calls)
	}
	if err = ins.CancelSubagent(calls[0].InvocationID); err != nil {
		t.Fatal(err)
	}
	collect(t, h)
	if calls = ins.Subagents(); calls[0].Status != "cancelled" || h.Status().Status != "completed" {
		t.Fatalf("child %+v parent %+v", calls, h.Status())
	}
}

func TestModelSwitchAndProjection(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Model.Name = "first"
	cfg.Model.Alternatives = []string{"second"}
	cfg.Model.APIKeyEnv = "GRAPE_TEST_MODEL_KEY"
	t.Setenv(cfg.Model.APIKeyEnv, "fixture")
	models := make(chan string, 3)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		models <- body.Model
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer s.Close()
	cfg.Model.BaseURL = s.URL
	rt, err := New(Options{Config: &cfg, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if got := rt.Models(); len(got) != 2 || got[1].Name != "second" || !got[0].KnownAdapter {
		t.Fatal(got)
	}
	for i, name := range []string{"first", "second"} {
		q := agent.RunRequest{UserID: "u", SessionID: "s", Message: strings.Repeat("x", i+1), ModelName: name}
		h, err := rt.Start(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		collect(t, h)
		select {
		case actual := <-models:
			if actual != name {
				t.Fatalf("wire model %q", actual)
			}
		default:
			t.Fatal("no HTTP request")
		}
		status, ok := h.(agent.RunInspector).ContextStatus()
		if !ok || status.Model != name || status.UserBytes < len(q.Message) || status.Tools == 0 || status.Messages < 1 {
			t.Fatalf("projection %+v ok=%v", status, ok)
		}
	}
}
