package config

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	"gopkg.in/yaml.v3"
)

var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv replaces ${NAME} and ${NAME:-default} in YAML text.
// An unset name without a default becomes an empty string.
func ExpandEnv(in []byte) []byte {
	return envPattern.ReplaceAllFunc(in, func(match []byte) []byte {
		parts := envPattern.FindSubmatch(match)
		name := string(parts[1])
		val, ok := os.LookupEnv(name)
		if ok {
			return []byte(val)
		}
		if len(parts) >= 3 && parts[2] != nil {
			return parts[2]
		}
		return []byte{}
	})
}

// Normalize fills built-in defaults on a configuration value.
func Normalize(c Config) Config {
	c.applyDefaults()
	return c
}

// Load merges defaults with the given YAML files. Missing files are errors.
// An empty path is skipped. Later files override earlier ones.
func Load(files ...string) (Config, error) {
	merged := map[string]any{}
	for _, file := range files {
		if strings.TrimSpace(file) == "" {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return Config{}, agent.Invalid("read config "+file, err)
		}
		overlay, err := decodeMap(ExpandEnv(raw))
		if err != nil {
			return Config{}, agent.Invalid("parse config "+file, err)
		}
		merged = mergeMaps(merged, overlay)
	}
	cfg, err := decodeConfig(merged)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeMap(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	var doc map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(false)
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func decodeConfig(merged map[string]any) (Config, error) {
	raw, err := yaml.Marshal(merged)
	if err != nil {
		return Config{}, agent.Invalid("encode config", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, agent.Invalid("config", err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

func mergeMaps(base, overlay map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		if v == nil {
			continue
		}
		if existing, ok := out[k]; ok {
			out[k] = mergeValue(existing, v)
			continue
		}
		out[k] = v
	}
	return out
}

func mergeValue(base, overlay any) any {
	bm, bok := base.(map[string]any)
	om, ook := overlay.(map[string]any)
	if bok && ook {
		return mergeMaps(bm, om)
	}
	return overlay
}

// Validate checks enumerated fields and duration syntax.
func (c Config) Validate() error {
	switch c.Runtime.Provider {
	case "", "trpc":
	default:
		return agent.Invalid("runtime.provider", fmt.Errorf("unsupported provider %q", c.Runtime.Provider))
	}
	switch c.Model.Provider {
	case "", "openai", "openai_compatible", "gemini", "anthropic",
		"deepseek", "qwen", "glm", "kimi", "minimax", "hunyuan", "huggingface":
	default:
		return agent.Invalid("model.provider", fmt.Errorf("unsupported provider %q", c.Model.Provider))
	}
	switch c.Session.Backend {
	case "", "sqlite":
	default:
		return agent.Invalid("session.backend", fmt.Errorf("unsupported backend %q", c.Session.Backend))
	}
	switch c.Workspace.Permission {
	case "", "read_only", "workspace_write", "danger_full_access":
	default:
		return agent.Invalid("workspace.permission", fmt.Errorf("unsupported permission %q", c.Workspace.Permission))
	}
	switch c.Sandbox.Mode {
	case "", "managed", "danger_full_access", "external":
	default:
		return agent.Invalid("sandbox.mode", fmt.Errorf("unsupported mode %q", c.Sandbox.Mode))
	}
	switch c.Sandbox.Network {
	case "", "restricted", "enabled":
	default:
		return agent.Invalid("sandbox.network", fmt.Errorf("unsupported network %q", c.Sandbox.Network))
	}
	switch c.Sandbox.DenyReadMode {
	case "", "add", "override":
	default:
		return agent.Invalid("sandbox.deny_read_mode", fmt.Errorf("unsupported mode %q", c.Sandbox.DenyReadMode))
	}
	switch c.Plugins.Conflict {
	case "", "error", "earliest", "latest":
	default:
		return agent.Invalid("plugins.conflict", fmt.Errorf("unsupported conflict policy %q", c.Plugins.Conflict))
	}
	for _, raw := range []string{c.Model.Timeout, c.Hooks.Timeout} {
		if _, err := Duration(raw); err != nil {
			return agent.Invalid("timeout", err)
		}
	}
	for _, srv := range c.MCP.Servers {
		if _, err := Duration(srv.Timeout); err != nil {
			return agent.Invalid("mcp.timeout", err)
		}
		if err := checkCredentialRef(srv.CredentialRef); err != nil {
			return err
		}
		for k, v := range srv.Headers {
			if err := checkHeaderValue(k, v); err != nil {
				return err
			}
		}
	}
	if c.Sandbox.Mode == "external" && strings.TrimSpace(c.Sandbox.Backend) == "" {
		return agent.Invalid("sandbox.backend", fmt.Errorf("external sandbox requires a named backend"))
	}
	return nil
}

func checkCredentialRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	if strings.HasPrefix(ref, "env:") && len(ref) > len("env:") {
		return nil
	}
	if strings.HasPrefix(ref, "file:") && len(ref) > len("file:") {
		return nil
	}
	return agent.Invalid("mcp.credential_ref", fmt.Errorf("credential_ref must be env:NAME or file:PATH"))
}

func checkHeaderValue(key, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "env:") || strings.HasPrefix(value, "file:") || strings.Contains(value, "${") {
		return nil
	}
	return agent.Invalid("mcp.headers."+key, fmt.Errorf("header values must be env:NAME, file:PATH, or ${NAME} references"))
}

// RedactedYAML returns YAML with secrets removed.
//
// Header values are masked. If model.api_key_env is set and that variable
// is present in the environment, its value is replaced wherever it appears.
func (c Config) RedactedYAML() string {
	clone := c
	clone.MCP.Servers = append([]MCPServerConfig(nil), c.MCP.Servers...)
	for i := range clone.MCP.Servers {
		if len(clone.MCP.Servers[i].Headers) == 0 {
			continue
		}
		masked := make(map[string]string, len(clone.MCP.Servers[i].Headers))
		for k := range clone.MCP.Servers[i].Headers {
			masked[k] = "***"
		}
		clone.MCP.Servers[i].Headers = masked
	}
	raw, err := yaml.Marshal(clone)
	if err != nil {
		return ""
	}
	out := string(raw)
	if c.Model.APIKeyEnv != "" {
		if secret := os.Getenv(c.Model.APIKeyEnv); secret != "" {
			out = strings.ReplaceAll(out, secret, "***")
		}
	}
	if c.Model.SecretIDEnv != "" {
		if secret := os.Getenv(c.Model.SecretIDEnv); secret != "" {
			out = strings.ReplaceAll(out, secret, "***")
		}
	}
	return out
}
