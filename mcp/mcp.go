package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Server is one configured MCP server. Header values and CredentialRef are
// env:NAME references, not secret material.
type Server struct {
	Name          string            `json:"name"`
	Transport     string            `json:"transport"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	URL           string            `json:"url,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	CredentialRef string            `json:"credential_ref,omitempty"`
	Enabled       bool              `json:"enabled"`
	Timeout       time.Duration     `json:"timeout,omitempty"`
	Source        string            `json:"source,omitempty"`
	// Trust is "host" when the operator accepts an unconfined stdio process.
	Trust string `json:"trust,omitempty"`
	// Level is "write" or "read". Empty means write.
	Level string `json:"level,omitempty"`
}

// Status is a connection snapshot. It does not include header values.
type Status struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	State     string `json:"state"`
	Detail    string `json:"detail,omitempty"`
	// Isolation is "none" for a stdio server, which runs on the host.
	Isolation string `json:"isolation,omitempty"`
}

// Connector opens one server. Implementations live in the runtime.
type Connector interface {
	Connect(ctx context.Context, server Server) (Status, error)
}

// Store is an in-memory server list with predictable merge order.
type Store struct {
	servers []Server
}

// Add inserts or replaces a server by name. The existing position is kept
// on replace so order stays stable.
func (s *Store) Add(server Server) error {
	if err := validate(server); err != nil {
		return err
	}
	for i := range s.servers {
		if s.servers[i].Name == server.Name {
			s.servers[i] = server
			return nil
		}
	}
	s.servers = append(s.servers, server)
	return nil
}

// Remove deletes a server by name.
func (s *Store) Remove(name string) error {
	for i := range s.servers {
		if s.servers[i].Name == name {
			s.servers = append(s.servers[:i], s.servers[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("mcp server %q not found", name)
}

// Enable changes the enabled bit.
func (s *Store) Enable(name string, enabled bool) error {
	for i := range s.servers {
		if s.servers[i].Name == name {
			s.servers[i].Enabled = enabled
			return nil
		}
	}
	return fmt.Errorf("mcp server %q not found", name)
}

// List returns the servers in insertion order.
func (s *Store) List() []Server {
	out := make([]Server, len(s.servers))
	for i, server := range s.servers {
		out[i] = redact(server)
	}
	return out
}

// Inspect returns one redacted server.
func (s *Store) Inspect(name string) (Server, error) {
	for _, server := range s.servers {
		if server.Name == name {
			return redact(server), nil
		}
	}
	return Server{}, fmt.Errorf("mcp server %q not found", name)
}

// Merge applies layers in order. Later layers replace the same name.
// The result keeps the first-seen position.
func Merge(layers ...[]Server) ([]Server, error) {
	var store Store
	for _, layer := range layers {
		for _, server := range layer {
			if err := store.Add(server); err != nil {
				return nil, err
			}
		}
	}
	return store.servers, nil
}

// ParseClaudeConfig reads a Claude-style .mcp.json object.
// Supported server fields are command, args, url, and headers.
// Other fields are ignored and are not treated as synonyms.
func ParseClaudeConfig(raw []byte, source string) ([]Server, error) {
	var doc struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Server
	for _, name := range names {
		item := doc.MCPServers[name]
		server := Server{
			Name:    name,
			Command: item.Command,
			Args:    item.Args,
			URL:     item.URL,
			Headers: item.Headers,
			Enabled: false,
			Source:  source,
		}
		if server.URL != "" {
			server.Transport = "sse"
		} else if server.Command != "" {
			server.Transport = "stdio"
		}
		if err := validate(server); err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, nil
}

// CheckStdio reports whether a stdio command may be launched.
// allow is the product permission check. A nil allow denies every command.
func CheckStdio(server Server, allow func(command string) error) error {
	if server.Transport != "stdio" && server.Transport != "" {
		return nil
	}
	if strings.TrimSpace(server.Command) == "" {
		return fmt.Errorf("stdio server %s has no command", server.Name)
	}
	if allow == nil {
		return fmt.Errorf("stdio server %s is not permitted", server.Name)
	}
	if err := allow(server.Command); err != nil {
		return fmt.Errorf("stdio server %s: %w", server.Name, err)
	}
	return nil
}

// Connect checks stdio permission before calling the connector.
func Connect(ctx context.Context, server Server, allow func(string) error, connector Connector) (Status, error) {
	if server.Transport == "stdio" || (server.Transport == "" && server.Command != "") {
		if err := CheckStdio(server, allow); err != nil {
			return Status{Name: server.Name, Transport: "stdio", State: "denied", Detail: err.Error()}, err
		}
	}
	if connector == nil {
		return Status{}, fmt.Errorf("mcp connector is nil")
	}
	return connector.Connect(ctx, server)
}

func validate(server Server) error {
	if strings.TrimSpace(server.Name) == "" {
		return fmt.Errorf("mcp server name is empty")
	}
	if err := checkRef(server.CredentialRef); err != nil {
		return fmt.Errorf("server %s: %w", server.Name, err)
	}
	for key, value := range server.Headers {
		if err := checkRef(value); err != nil && strings.TrimSpace(value) != "" {
			return fmt.Errorf("server %s header %s: %w", server.Name, key, err)
		}
	}
	return nil
}

func checkRef(value string) error {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return nil
	case strings.HasPrefix(value, "env:") && len(value) > len("env:"):
		return nil
	case strings.HasPrefix(value, "file:") && len(value) > len("file:"):
		return nil
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") && len(value) > len("${}"):
		return nil
	default:
		return fmt.Errorf("value must be an env:NAME, file:PATH, or ${NAME} reference")
	}
}

// ResolveRef expands env:, file:, and ${NAME} credential references.
// A missing target is an error that names the reference and not a secret.
func ResolveRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		return "", nil
	case strings.HasPrefix(value, "env:"):
		name := strings.TrimPrefix(value, "env:")
		got, ok := os.LookupEnv(name)
		if !ok || got == "" {
			return "", fmt.Errorf("missing env:%s", name)
		}
		return got, nil
	case strings.HasPrefix(value, "file:"):
		path := strings.TrimPrefix(value, "file:")
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("missing file:%s", path)
		}
		return strings.TrimSpace(string(raw)), nil
	case strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}"):
		name := strings.TrimSuffix(strings.TrimPrefix(value, "${"), "}")
		got, ok := os.LookupEnv(name)
		if !ok || got == "" {
			return "", fmt.Errorf("missing ${%s}", name)
		}
		return got, nil
	default:
		return value, nil
	}
}

// ResolveHeaders resolves every header reference.
func ResolveHeaders(headers map[string]string) (map[string]string, []string, error) {
	if len(headers) == 0 {
		return nil, nil, nil
	}
	out := make(map[string]string, len(headers))
	var secrets []string
	for key, value := range headers {
		resolved, err := ResolveRef(value)
		if err != nil {
			return nil, nil, fmt.Errorf("header %s: %w", key, err)
		}
		out[key] = resolved
		if resolved != "" && resolved != value {
			secrets = append(secrets, resolved)
		}
	}
	return out, secrets, nil
}

// PluginAllowed reports whether plugin/server is in the allow list.
func PluginAllowed(allow []string, key string) bool {
	for _, item := range allow {
		if item == key {
			return true
		}
	}
	return false
}

func redact(server Server) Server {
	if len(server.Headers) == 0 {
		return server
	}
	masked := make(map[string]string, len(server.Headers))
	for key := range server.Headers {
		masked[key] = "env:***"
	}
	server.Headers = masked
	return server
}
