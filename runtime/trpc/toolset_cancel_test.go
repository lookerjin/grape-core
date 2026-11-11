package trpcruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
	mcp "trpc.group/trpc-go/trpc-mcp-go"
)

// A provider can continue working after its caller sees cancellation. Hold
// its handler open and record whether the next turn starts before release.
func TestToolSetCancelAndReuse(t *testing.T) {
	for _, remote := range []bool{true, false} {
		name := "trusted-child"
		if remote {
			name = "mcp-http"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			block := func() { close(started); <-release; close(finished) }
			cfg := quietConfig(t)
			cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
			findBlock := func(ctx context.Context, req *model.Request) (*model.Response, error) {
				for key := range req.Tools {
					if strings.HasSuffix(key, "hold") {
						return mockmodel.ToolCall(key, "{}"), nil
					}
				}
				return nil, fmt.Errorf("hold tool missing")
			}
			opts := Options{Config: &cfg, DataDir: t.TempDir()}
			if remote {
				server := mcp.NewServer("fixture", "1")
				server.RegisterTool(mcp.NewTool("hold", mcp.WithDescription("Wait for release"), mcp.WithInputStruct[struct{}]()), mcp.NewTypedToolHandler(func(context.Context, *mcp.CallToolRequest, struct{}) (echoOut, error) {
					block()
					return echoOut{Text: "released"}, nil
				}))
				srv := httptest.NewServer(server.HTTPHandler())
				defer srv.Close()
				cfg.MCP.Strict = true
				cfg.MCP.Servers = []config.MCPServerConfig{{Name: "fixture", Transport: "streamable", URL: srv.URL + "/mcp"}}
			} else {
				local := function.NewFunctionTool(func(context.Context, struct{}) (string, error) { block(); return "released", nil }, function.WithName("hold"))
				opts.Subagents = []SubagentSpec{{Name: "worker", TrustToolSets: true, ToolSets: []tool.ToolSet{dynSet{tool: local}}, Model: mockmodel.New("worker", findBlock)}}
			}
			opts.Model = mockmodel.New("main", func(ctx context.Context, req *model.Request) (*model.Response, error) {
				if lastUserText(req) == "again" {
					return mockmodel.Echo(ctx, req)
				}
				if remote {
					return findBlock(ctx, req)
				}
				return mockmodel.ToolCall("worker", `{"request":"hold"}`), nil
			})
			rt, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.Close()
			releaseProvider := sync.OnceFunc(func() { close(release) })
			defer releaseProvider()
			req := agent.RunRequest{UserID: "u", SessionID: "same", Message: "start"}
			h, err := rt.Start(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("provider never started")
			}
			if err := h.Cancel(); err != nil {
				t.Fatal(err)
			}
			req.Message = "again"
			startAgain := func(timeout time.Duration) (agent.RunHandle, error) {
				nextCtx, end := context.WithTimeout(ctx, timeout)
				t.Cleanup(end)
				for {
					next, err := rt.Start(nextCtx, req)
					if !errors.Is(err, agent.ErrSession) || nextCtx.Err() != nil {
						return next, err
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			next, err := startAgain(500 * time.Millisecond)
			if err != nil {
				if remote || !(errors.Is(err, agent.ErrSession) || errors.Is(err, agent.ErrRunCancelled)) {
					t.Fatalf("reuse with held provider: %v", err)
				}
				// A child stream may stay tracked until its provider returns.
				t.Log("child drain still held the session during the observation window")
				releaseProvider()
				next, err = startAgain(2 * time.Second)
				if err != nil {
					t.Fatalf("reuse after releasing provider: %v", err)
				}
			}
			collect(t, next)
			if next.Status().Status != "completed" {
				t.Fatal(next.Status())
			}
			select {
			case <-finished:
				t.Log("second turn completed after provider release")
			default:
				t.Log("second turn completed while original provider was still held")
			}
		})
	}
}
