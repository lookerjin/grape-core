// Package config loads, merges, and validates grape-core YAML configuration.
//
// Environment placeholders use ${NAME} or ${NAME:-default}. They are expanded
// before YAML decoding. A bare $NAME is left unchanged. Unknown YAML fields
// are errors.
//
// Merge order is built-in defaults, then each file passed to Load, later
// files winning. Nested maps merge. Scalars and arrays are replaced when the
// later file sets the key. Explicit false is preserved.
package config

import (
	"time"
)

// Config is the effective grape-core configuration.
type Config struct {
	Runtime   RuntimeConfig   `yaml:"runtime"`
	Model     ModelConfig     `yaml:"model"`
	Session   SessionConfig   `yaml:"session"`
	Workspace WorkspaceConfig `yaml:"workspace"`
	Sandbox   SandboxConfig   `yaml:"sandbox"`
	Skills    SkillsConfig    `yaml:"skills"`
	Plugins   PluginsConfig   `yaml:"plugins"`
	MCP       MCPConfig       `yaml:"mcp"`
	Memory    MemoryConfig    `yaml:"memory"`
	Evolution EvolutionConfig `yaml:"evolution"`
	Hooks     HooksConfig     `yaml:"hooks"`
	Journal   JournalConfig   `yaml:"journal"`
	Goal      GoalConfig      `yaml:"goal"`
	Plan      PlanConfig      `yaml:"plan"`
	Context   ContextConfig   `yaml:"context"`
	Approvals ApprovalConfig  `yaml:"approvals"`
}

// RuntimeConfig selects the agent runtime. Only "trpc" is implemented.
type RuntimeConfig struct {
	Provider string `yaml:"provider"`
}

// ModelConfig points at a model. The API key is the name of an environment
// variable, never the key itself.
type ModelConfig struct {
	Provider    string           `yaml:"provider"`
	Name        string           `yaml:"name"`
	BaseURL     string           `yaml:"base_url"`
	APIKeyEnv   string           `yaml:"api_key_env"`
	SecretIDEnv string           `yaml:"secret_id_env"`
	Timeout     string           `yaml:"timeout"`
	Generation  GenerationConfig `yaml:"generation"`
}

// GenerationConfig is a small subset of sampling controls.
type GenerationConfig struct {
	Temperature *float64 `yaml:"temperature"`
	MaxTokens   *int     `yaml:"max_tokens"`
	TopP        *float64 `yaml:"top_p"`
	Stream      *bool    `yaml:"stream"`
}

// SessionConfig selects session storage. Backend "sqlite" is the default.
type SessionConfig struct {
	Backend    string `yaml:"backend"`
	Path       string `yaml:"path"`
	EventLimit int    `yaml:"event_limit"`
}

// WorkspaceConfig is the project the agent may read or edit.
type WorkspaceConfig struct {
	Root       string `yaml:"root"`
	Permission string `yaml:"permission"`
}

// SandboxConfig selects isolation. Mode "managed" asks for the OS sandbox.
//
// DenyRead adds or replaces the default sensitive-path list. DenyReadMode is
// "add" (default) or "override". Entries with a glob metacharacter are
// workspace-relative and are passed to WithNoAccessGlobs. Other entries are
// host paths passed to WithNoAccessPaths when they exist. See docs/sandbox.md.
type SandboxConfig struct {
	Mode         string   `yaml:"mode"`
	Network      string   `yaml:"network"`
	Backend      string   `yaml:"backend"`
	DenyRead     []string `yaml:"deny_read"`
	DenyReadMode string   `yaml:"deny_read_mode"`
}

// SkillsConfig lists skill roots. KnowledgeOnly defaults to true.
type SkillsConfig struct {
	Paths         []string `yaml:"paths"`
	KnowledgeOnly *bool    `yaml:"knowledge_only"`
}

// PluginsConfig lists plugin roots and which installed plugins are enabled.
// Conflict is "error" (default), "earliest", or "latest".
type PluginsConfig struct {
	Dirs     []string `yaml:"dirs"`
	Enabled  []string `yaml:"enabled"`
	Conflict string   `yaml:"conflict"`
}

// MCPServerConfig is one MCP server. Header values and credentials are
// references such as "env:NAME", not secret material.
type MCPServerConfig struct {
	Name          string            `yaml:"name"`
	Transport     string            `yaml:"transport"`
	Command       string            `yaml:"command"`
	Args          []string          `yaml:"args"`
	URL           string            `yaml:"url"`
	Headers       map[string]string `yaml:"headers"`
	CredentialRef string            `yaml:"credential_ref"`
	Enabled       *bool             `yaml:"enabled"`
	Timeout       string            `yaml:"timeout"`
}

// MCPConfig is the ordered server list. Later files replace the whole list
// when they set mcp.servers.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
}

// MemoryConfig enables the Markdown memory root. It is off by default.
type MemoryConfig struct {
	Enabled bool   `yaml:"enabled"`
	Root    string `yaml:"root"`
}

// EvolutionConfig enables the optional evolution worker. It is off by default.
type EvolutionConfig struct {
	Enabled bool   `yaml:"enabled"`
	Root    string `yaml:"root"`
}

// HooksConfig bounds external hook execution.
type HooksConfig struct {
	Timeout       string `yaml:"timeout"`
	MaxConcurrent int    `yaml:"max_concurrent"`
}

// JournalConfig optionally writes a redacted NDJSON journal.
type JournalConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Path          string `yaml:"path"`
	MaxFieldBytes int    `yaml:"max_field_bytes"`
}

// GoalConfig bounds goal mode. Zero MaxIterations means the default.
type GoalConfig struct {
	MaxIterations int    `yaml:"max_iterations"`
	Promise       string `yaml:"promise"`
}

// PlanConfig controls whether plan mode requires an approval before writes.
type PlanConfig struct {
	RequireApproval *bool `yaml:"require_approval"`
}

// ContextConfig bounds one run. Negative values mean unlimited.
type ContextConfig struct {
	MaxLLMCalls       int `yaml:"max_llm_calls"`
	MaxToolIterations int `yaml:"max_tool_iterations"`
}

// ApprovalConfig requires an approval for mutating tools when true.
type ApprovalConfig struct {
	Required bool `yaml:"required"`
}

// Default returns configuration with built-in defaults applied.
func Default() Config {
	c := Config{}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Runtime.Provider == "" {
		c.Runtime.Provider = "trpc"
	}
	if c.Model.Provider == "" {
		c.Model.Provider = "openai_compatible"
	}
	if c.Session.Backend == "" {
		c.Session.Backend = "sqlite"
	}
	if c.Workspace.Permission == "" {
		c.Workspace.Permission = "read_only"
	}
	if c.Sandbox.Mode == "" {
		c.Sandbox.Mode = "managed"
	}
	if c.Sandbox.Network == "" {
		c.Sandbox.Network = "restricted"
	}
	if c.Sandbox.DenyReadMode == "" {
		c.Sandbox.DenyReadMode = "add"
	}
	if c.Plugins.Conflict == "" {
		c.Plugins.Conflict = "error"
	}
	if c.Skills.KnowledgeOnly == nil {
		v := true
		c.Skills.KnowledgeOnly = &v
	}
	if c.Model.Generation.Stream == nil {
		v := true
		c.Model.Generation.Stream = &v
	}
	if c.Hooks.Timeout == "" {
		c.Hooks.Timeout = "30s"
	}
	if c.Hooks.MaxConcurrent == 0 {
		c.Hooks.MaxConcurrent = 4
	}
	if c.Journal.MaxFieldBytes == 0 {
		c.Journal.MaxFieldBytes = 8192
	}
	if c.Goal.MaxIterations == 0 {
		c.Goal.MaxIterations = 8
	}
	if c.Goal.Promise == "" {
		c.Goal.Promise = "<promise>DONE</promise>"
	}
	if c.Plan.RequireApproval == nil {
		v := true
		c.Plan.RequireApproval = &v
	}
	if c.Context.MaxLLMCalls == 0 {
		c.Context.MaxLLMCalls = 32
	}
	if c.Context.MaxToolIterations == 0 {
		c.Context.MaxToolIterations = 24
	}
}

// KnowledgeOnly reports whether skill execution tools stay disabled.
func (c Config) KnowledgeOnly() bool {
	if c.Skills.KnowledgeOnly == nil {
		return true
	}
	return *c.Skills.KnowledgeOnly
}

// PlanRequiresApproval reports whether plan mode must approve mutations.
func (c Config) PlanRequiresApproval() bool {
	if c.Plan.RequireApproval == nil {
		return true
	}
	return *c.Plan.RequireApproval
}

// Duration parses a timeout field. Empty is zero.
func Duration(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	return time.ParseDuration(raw)
}
