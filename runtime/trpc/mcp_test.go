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
	cfg.MCP.Servers = []config.MCPServerConfig{{
		Name: "echo", Transport: "stdio", Command: bin,
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
	deniedCfg.Workspace.Permission = agent.PermissionReadOnly
	yes := true
	deniedCfg.MCP.Servers[0].Enabled = &yes
	if _, err := New(Options{Config: &deniedCfg, Model: mockmodel.New("deny", mockmodel.Echo), DataDir: t.TempDir()}); err == nil {
		t.Fatal("expected denied stdio server to fail before start")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("denied MCP server started")
	}
}
