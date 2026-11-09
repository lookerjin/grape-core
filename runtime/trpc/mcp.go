package trpcruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/mcp"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	trpcmcp "trpc.group/trpc-go/trpc-agent-go/tool/mcp"
)

func (rt *Runtime) allowMCPCommand(command string) error {
	if rt.cfg.Workspace.Permission == "read_only" {
		return fmt.Errorf("read_only denies stdio MCP commands")
	}
	return nil
}

func serversFromConfig(cfg config.Config) ([]mcp.Server, error) {
	var out []mcp.Server
	for _, item := range cfg.MCP.Servers {
		enabled := true
		if item.Enabled != nil {
			enabled = *item.Enabled
		}
		timeout, err := config.Duration(item.Timeout)
		if err != nil {
			return nil, err
		}
		out = append(out, mcp.Server{
			Name:          item.Name,
			Transport:     item.Transport,
			Command:       item.Command,
			Args:          append([]string(nil), item.Args...),
			URL:           item.URL,
			Headers:       item.Headers,
			CredentialRef: item.CredentialRef,
			Enabled:       enabled,
			Timeout:       timeout,
			Source:        "config",
		})
	}
	return out, nil
}

func (rt *Runtime) openMCP(ctx context.Context) error {
	for _, server := range rt.mcpServers {
		if !server.Enabled || !stdioServer(server) {
			continue
		}
		if err := mcp.CheckStdio(server, rt.allowMCPCommand); err != nil {
			return err
		}
	}
	var sets []tool.ToolSet
	for _, server := range rt.mcpServers {
		if !server.Enabled {
			continue
		}
		set, err := dialMCP(ctx, server)
		if err != nil {
			for _, opened := range sets {
				_ = opened.Close()
			}
			return err
		}
		sets = append(sets, set)
	}
	rt.mcpSets = sets
	return nil
}

func stdioServer(server mcp.Server) bool {
	return server.Transport == "" || server.Transport == "stdio" || server.Command != "" && server.URL == ""
}

func dialMCP(ctx context.Context, server mcp.Server) (tool.ToolSet, error) {
	cfg := trpcmcp.ConnectionConfig{
		Transport: server.Transport,
		Command:   server.Command,
		Args:      server.Args,
		ServerURL: server.URL,
		Headers:   server.Headers,
		Timeout:   server.Timeout,
	}
	if cfg.Transport == "" {
		if server.URL != "" {
			cfg.Transport = "sse"
		} else {
			cfg.Transport = "stdio"
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 20 * time.Second
	}
	set := trpcmcp.NewMCPToolSet(cfg, trpcmcp.WithName(server.Name))
	if err := set.Init(ctx); err != nil {
		_ = set.Close()
		return nil, fmt.Errorf("mcp %s: %w", server.Name, err)
	}
	return set, nil
}
