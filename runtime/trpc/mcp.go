package trpcruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/redact"
	"github.com/lookerjin/grape-core/mcp"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	trpcmcp "trpc.group/trpc-go/trpc-agent-go/tool/mcp"
)

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
			Trust:         item.Trust,
			Level:         item.Level,
		})
	}
	return out, nil
}

func (rt *Runtime) openMCP(ctx context.Context) error {
	var sets []tool.ToolSet
	for _, server := range rt.mcpServers {
		if !server.Enabled {
			rt.mcpStatus = append(rt.mcpStatus, mcp.Status{
				Name: server.Name, Transport: transportOf(server), State: "disabled",
			})
			continue
		}
		if isStdio(server) && server.Trust != "host" {
			rt.mcpStatus = append(rt.mcpStatus, mcp.Status{
				Name: server.Name, Transport: "stdio", State: "denied",
				Detail: "stdio MCP requires trust: host", Isolation: "none",
			})
			continue
		}
		set, err := rt.dialMCP(ctx, server)
		if err != nil {
			redacted := redactDetail(err.Error(), rt.secrets)
			rt.mcpStatus = append(rt.mcpStatus, mcp.Status{
				Name: server.Name, Transport: transportOf(server), State: "failed",
				Detail: redacted, Isolation: isolationOf(server),
			})
			if rt.cfg.MCP.Strict {
				for _, opened := range sets {
					_ = opened.Close()
				}
				return fmt.Errorf("%s", redacted)
			}
			continue
		}
		sets = append(sets, hostMCPSet{inner: set, rt: rt, kind: mcpKind(server)})
		rt.mcpStatus = append(rt.mcpStatus, mcp.Status{
			Name: server.Name, Transport: transportOf(server), State: "connected",
			Isolation: isolationOf(server),
		})
	}
	rt.mcpSets = sets
	return nil
}

func redactDetail(detail string, secrets []string) string {
	return redact.Text(detail, secrets)
}

func isStdio(server mcp.Server) bool {
	switch strings.ToLower(strings.TrimSpace(server.Transport)) {
	case "sse", "streamable", "streamable_http", "http":
		return false
	case "stdio":
		return true
	case "":
		return strings.TrimSpace(server.URL) == ""
	default:
		return strings.TrimSpace(server.URL) == ""
	}
}

func transportOf(server mcp.Server) string {
	if t := strings.TrimSpace(server.Transport); t != "" {
		return t
	}
	if strings.TrimSpace(server.URL) != "" {
		return inferHTTPTransport(server.URL)
	}
	return "stdio"
}

func inferHTTPTransport(rawURL string) string {
	lower := strings.ToLower(rawURL)
	if strings.Contains(lower, "sse") {
		return "sse"
	}
	return "streamable"
}

func isolationOf(server mcp.Server) string {
	if isStdio(server) {
		return "none"
	}
	return ""
}

func (rt *Runtime) headerPolicy(server mcp.Server) mcp.HeaderPolicy {
	policy := mcp.HeaderPolicy{}
	if server.PluginKey == "" {
		return policy
	}
	policy.Plugin = true
	policy.AllowEnv = map[string]struct{}{}
	for _, name := range rt.cfg.MCP.PluginEnvAllow[server.PluginKey] {
		policy.AllowEnv[name] = struct{}{}
	}
	return policy
}

func (rt *Runtime) dialMCP(ctx context.Context, server mcp.Server) (tool.ToolSet, error) {
	policy := rt.headerPolicy(server)
	headers, secrets, err := mcp.ResolveHeaders(server.Headers, policy)
	if err != nil {
		return nil, fmt.Errorf("mcp %s: %w", server.Name, err)
	}
	if ref := strings.TrimSpace(server.CredentialRef); ref != "" {
		if _, exists := headers["Authorization"]; exists {
			return nil, fmt.Errorf("mcp %s: credential_ref is set but Authorization is already set", server.Name)
		}
		value, err := mcp.ResolveRef(ref, policy)
		if err != nil {
			return nil, fmt.Errorf("mcp %s: %w", server.Name, err)
		}
		if headers == nil {
			headers = map[string]string{}
		}
		if value != "" {
			headers["Authorization"] = value
			secrets = append(secrets, value)
		}
	}
	rt.secrets = append(rt.secrets, secrets...)
	cfg := trpcmcp.ConnectionConfig{
		Transport: transportOf(server),
		Command:   server.Command,
		Args:      server.Args,
		ServerURL: server.URL,
		Headers:   headers,
		Timeout:   server.Timeout,
	}
	if cfg.Transport == "stdio" {
		cfg.Command, cfg.Args = minimalStdioEnv(server.Command, server.Args)
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

// minimalStdioEnv launches the server via env -i so the process does not
// inherit the host environment. The upstream stdio client prepends
// os.Environ whenever its own Env map is set, so the clear has to happen
// in the executed command.
func minimalStdioEnv(command string, args []string) (string, []string) {
	if runtime.GOOS == "windows" || strings.TrimSpace(command) == "" {
		return command, args
	}
	envBin, err := exec.LookPath("env")
	if err != nil {
		envBin = "/usr/bin/env"
	}
	argv := []string{"-i"}
	for _, key := range []string{"PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ", "USER", "LOGNAME", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			argv = append(argv, key+"="+value)
		}
	}
	argv = append(argv, command)
	argv = append(argv, args...)
	return envBin, argv
}

// hostMCPSet marks every stdio tool result with isolation none so the
// event stream records that the call ran on the host.
func mcpKind(server mcp.Server) string {
	if strings.EqualFold(strings.TrimSpace(server.Level), "read") {
		return "read"
	}
	return "write"
}

type hostMCPSet struct {
	inner tool.ToolSet
	rt    *Runtime
	kind  string
}

func (s hostMCPSet) Tools(ctx context.Context) []tool.Tool {
	inner := s.inner.Tools(ctx)
	out := make([]tool.Tool, 0, len(inner))
	for _, item := range inner {
		if callable, ok := item.(tool.CallableTool); ok {
			wrapped := &hostMCPTool{inner: callable, rt: s.rt, kind: s.kind}
			if s.kind == "read" {
				s.rt.noteRead(wrapped)
			}
			out = append(out, wrapped)
			continue
		}
		out = append(out, item)
	}
	return out
}

func (s hostMCPSet) Close() error { return s.inner.Close() }

func (s hostMCPSet) Name() string { return s.inner.Name() }

type hostMCPTool struct {
	inner tool.CallableTool
	rt    *Runtime
	kind  string
}

func (t hostMCPTool) Declaration() *tool.Declaration { return t.inner.Declaration() }

func (t hostMCPTool) Call(ctx context.Context, args []byte) (any, error) {
	if st := stateFrom(ctx); st != nil {
		st.enterTool()
		defer st.leaveTool()
	}
	if t.rt != nil {
		name := ""
		if decl := t.Declaration(); decl != nil {
			name = decl.Name
		}
		if err := t.rt.authorize(ctx, t.kind, name, ""); err != nil {
			return map[string]any{"isolation": "none", "denied": true, "error": err.Error()}, nil
		}
	}
	res, err := t.inner.Call(ctx, args)
	body := map[string]any{"isolation": "none", "result": res}
	if err != nil {
		body["error"] = err.Error()
		return body, err
	}
	return body, nil
}
