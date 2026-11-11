package trpcruntime

import (
	"context"
	"errors"
	"github.com/lookerjin/grape-core/mcp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestMCPStdioToolCallAndDeniedServerNotStarted(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "mcpstdio")
	build := exec.Command("go", "build", "-o", bin, "./testdata/mcpstdio")
	build.Dir = moduleRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=1", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build server: %v\n%s", err, out)
	}
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	t.Setenv("FAKE_API_KEY", "mcp-secret-key")
	cfg.MCP.Servers = []config.MCPServerConfig{{
		Name: "echo", Transport: "stdio", Command: bin, Trust: "host",
	}}
	mdl := mockmodel.New("mcp", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		name := "echo"
		if req != nil {
			for toolName := range req.Tools {
				if strings.Contains(toolName, "echo") {
					name = toolName
					break
				}
			}
		}
		return &model.Response{Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-mcp",
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      name,
						Arguments: []byte(`{"message":"ping"}`),
					},
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
		UserID: "u", SessionID: "mcp", Message: "call the echo tool",
	})
	if err != nil {
		t.Fatal(err)
	}
	events := collect(t, h)
	blob := ""
	for _, evt := range events {
		blob += string(evt.Payload)
	}
	if !strings.Contains(blob, "echo:ping") {
		t.Fatalf("mcp tool result missing: %s", blob)
	}
	if strings.Contains(blob, "mcp-secret-key") {
		t.Fatal("stdio MCP inherited FAKE_API_KEY")
	}
	if !strings.Contains(blob, `isolation\":\"none`) && !strings.Contains(blob, `"isolation":"none"`) {
		t.Fatalf("event stream missing isolation none: %s", blob)
	}
	var sawNone bool
	for _, status := range rt.MCPStatus() {
		if status.Name == "echo" && status.State == "connected" && status.Isolation == "none" {
			sawNone = true
		}
	}
	if !sawNone {
		t.Fatalf("status missing isolation none: %+v", rt.MCPStatus())
	}

	ctx := context.Background()
	old := rt.snapshotMCP()[0].Tools(ctx)[0].(*hostMCPTool)
	if err := rt.RestartMCP(ctx, "echo"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Call(ctx, []byte("{}")); err == nil {
		t.Fatal("retired tool executed")
	}
	if err := rt.RemoveMCP(ctx, "echo"); err != nil || len(rt.MCPStatus()) != 0 {
		t.Fatal(err)
	}
	spec := mcp.Server{Name: "echo", Transport: "stdio", Command: bin, Enabled: true}
	if err := rt.SetMCP(ctx, spec); !errors.Is(err, agent.ErrPermissionDenied) {
		t.Fatal(err)
	}
	spec.Trust = "host"
	if err := rt.SetMCP(ctx, spec); err != nil {
		t.Fatal(err)
	}
	status := rt.MCPStatus()
	if len(status) != 1 || status[0].Version < 3 || len(status[0].Tools) == 0 {
		t.Fatal(status)
	}
	h, err = rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "reloaded", Message: "call echo"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(eventBlob(collect(t, h)), "echo:ping") {
		t.Fatal("reloaded MCP tool failed")
	}
	marker := filepath.Join(t.TempDir(), "started")
	enabled := false
	disabled := config.Default()
	disabled.Workspace.Permission = agent.PermissionWorkspaceWrite
	disabled.Journal.Enabled = false
	disabled.Model.Generation.Stream = boolPtr(false)
	disabled.MCP.Servers = []config.MCPServerConfig{{
		Name: "nope", Transport: "stdio", Command: "sh",
		Args:    []string{"-c", "echo started > " + marker},
		Enabled: &enabled,
	}}
	off, err := New(Options{Config: &disabled, Model: mockmodel.New("off", mockmodel.Echo), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	off.Close()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("disabled MCP server started")
	}

	deniedCfg := disabled
	deniedCfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	yes := true
	deniedCfg.MCP.Servers[0].Enabled = &yes
	deniedCfg.MCP.Servers[0].Trust = ""
	denied, err := New(Options{Config: &deniedCfg, Model: mockmodel.New("deny", mockmodel.Echo), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Close()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("untrusted MCP server started")
	}
	var deniedState string
	for _, status := range denied.MCPStatus() {
		if status.Name == "nope" {
			deniedState = status.State
		}
	}
	if deniedState != "denied" {
		t.Fatalf("untrusted server state %q", deniedState)
	}

	urlCfg := config.Default()
	urlCfg.Journal.Enabled = false
	urlCfg.Model.Generation.Stream = boolPtr(false)
	urlCfg.MCP.Servers = []config.MCPServerConfig{{
		Name: "remote", URL: "http://127.0.0.1:1/mcp",
	}}
	urlRT, err := New(Options{Config: &urlCfg, Model: mockmodel.New("url", mockmodel.Echo), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer urlRT.Close()
	for _, status := range urlRT.MCPStatus() {
		if status.Name != "remote" {
			continue
		}
		if status.Transport == "stdio" || strings.Contains(status.Detail, "has no command") {
			t.Fatalf("URL server classified as stdio: %+v", status)
		}
		if status.State != "failed" {
			t.Fatalf("dead URL server state %q", status.State)
		}
	}
}
