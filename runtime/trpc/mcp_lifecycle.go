package trpcruntime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/mcp"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func (rt *Runtime) secretSnapshot() []string {
	rt.secretMu.RLock()
	defer rt.secretMu.RUnlock()
	return rt.secrets
}
func (rt *Runtime) snapshotMCP() []tool.ToolSet {
	rt.mcpMu.RLock()
	defer rt.mcpMu.RUnlock()
	return slices.Clone(rt.mcpSets)
}

// SetMCP adds or atomically reloads a trusted application server definition.
// Failed replacements leave the old generation callable.
func (rt *Runtime) SetMCP(ctx context.Context, s mcp.Server) error {
	return rt.changeMCP(ctx, s.Name, &s, false)
}
func (rt *Runtime) RestartMCP(ctx context.Context, name string) error {
	return rt.changeMCP(ctx, name, nil, true)
}
func (rt *Runtime) RemoveMCP(ctx context.Context, name string) error {
	return rt.changeMCP(ctx, name, nil, false)
}

func (rt *Runtime) changeMCP(ctx context.Context, name string, next *mcp.Server, restart bool) error {
	rt.mcpOp.Lock()
	defer rt.mcpOp.Unlock()
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return agent.ErrSession
	}
	rt.starting.Add(1)
	rt.mu.Unlock()
	defer rt.starting.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(rt.lifetime, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	var prior *mcp.Server
	for _, s := range rt.mcpServers {
		if s.Name == name {
			prior = &s
			break
		}
	}
	if next == nil && prior == nil {
		return fmt.Errorf("unknown MCP server %q", name)
	}
	if next != nil && (next.PluginKey != "" || (prior != nil && prior.PluginKey != "")) {
		return agent.ErrPermissionDenied
	}
	if restart {
		next = prior
	}
	var live *hostMCPSet
	status := mcp.Status{Name: name, State: "disabled"}
	if next != nil {
		if _, err := mcp.Merge([]mcp.Server{*next}); err != nil {
			return err
		}
		next.Args = slices.Clone(next.Args)
		next.Headers = maps.Clone(next.Headers)
		status.Transport = transportOf(*next)
		status.Isolation = isolationOf(*next)
		if next.Enabled {
			switch status.Transport {
			case "stdio", "sse", "streamable":
			default:
				return errors.New("unsupported MCP transport")
			}
			if isStdio(*next) && next.Trust != "host" {
				return agent.ErrPermissionDenied
			}
			set, e := rt.dialMCP(ctx, *next)
			if e != nil {
				return errors.New(redactDetail(e.Error(), rt.secretSnapshot()))
			}
			live, status.Tools = newLiveMCP(ctx, set, rt, mcpKind(*next))
			status.State = "connected"
		}
	}
	if e := ctx.Err(); e != nil {
		if live != nil {
			_ = live.Close()
		}
		return e
	}
	rt.mcpMu.Lock()
	rt.mcpVersion++
	status.Version = rt.mcpVersion
	var retired tool.ToolSet
	rt.mcpSets = slices.DeleteFunc(rt.mcpSets, func(s tool.ToolSet) bool {
		if s.Name() == name {
			retired = s
			return true
		}
		return false
	})
	rt.mcpServers = slices.DeleteFunc(rt.mcpServers, func(s mcp.Server) bool { return s.Name == name })
	rt.mcpStatus = slices.DeleteFunc(rt.mcpStatus, func(s mcp.Status) bool { return s.Name == name })
	if next != nil {
		rt.mcpServers = append(rt.mcpServers, *next)
		rt.mcpStatus = append(rt.mcpStatus, status)
		if live != nil {
			rt.mcpSets = append(rt.mcpSets, live)
		}
	}
	rt.mcpMu.Unlock()
	if retired != nil {
		return retired.Close()
	}
	return nil
}
