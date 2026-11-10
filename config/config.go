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
	// MaxReadBytes caps read_file and each file search_content opens.
	// Zero uses 1 MiB.
	MaxReadBytes int `yaml:"max_read_bytes"`
	// MaxSearchHits caps search_content matches. Zero uses 100.
	MaxSearchHits int `yaml:"max_search_hits"`
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
	// EnvAllow replaces the managed-sandbox environment allowlist.
	// Empty keeps the core list: PATH, HOME, LANG, TERM, and the other
	// shell startup variables.
	EnvAllow []string `yaml:"env_allow"`
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
	// Trust must be "host" before a stdio server is launched. Stdio MCP
	// runs on the host with isolation none; it is not the managed sandbox.
	Trust string `yaml:"trust"`
	// Level is "write" (default) or "read". Write-level MCP tools are gated
	// like other workspace writes.
	Level string `yaml:"level"`
}

// MCPConfig is the ordered server list. Later files replace the whole list
// when they set mcp.servers.
type MCPConfig struct {
	Servers []MCPServerConfig `yaml:"servers"`
	// Strict makes one failed MCP server fail Runtime construction.
	// The default reports that server as failed and continues.
	Strict bool `yaml:"strict"`
	// AllowPluginServers lists plugin/server keys that may be enabled.
	// An entry may be a name or {name, trust}. trust: host is the only
	// way to launch a plugin stdio server. Plugin files cannot grant it.
	AllowPluginServers PluginGrants `yaml:"allow_plugin_servers"`
	// PluginEnvAllow maps "plugin/server" to env var names that plugin
	// headers may read. file: refs from plugins are never resolved.
	PluginEnvAllow map[string][]string `yaml:"plugin_env_allow"`
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
	// Allow lists plugin component keys, such as "name/hooks", that may
	// execute. Installing or enabling a plugin does not grant this.
	// An empty list runs no plugin hooks. Embedder-supplied hooks are a
	// separate grant on runtime Options.
	Allow []string `yaml:"allow"`
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

// ContextConfig bounds one run.
// A nil limit uses the documented default. Zero and negative mean unlimited.
type ContextConfig struct {
	// MaxLLMCalls is the per-agent model-call limit passed to tRPC.
	// Nil uses 128. The framework resets this counter on each invocation.
	MaxLLMCalls *int `yaml:"max_llm_calls"`
	// MaxToolIterations is the per-agent limit on model turns that request
	// tools, one round however many calls it contains. Nil uses 64.
	MaxToolIterations *int `yaml:"max_tool_iterations"`
	// TotalMaxLLMCalls bounds model calls for the whole run, including
	// nested agents. Nil uses 128. tRPC has no cross-invocation total.
	TotalMaxLLMCalls *int `yaml:"total_max_llm_calls"`
	// TotalMaxToolIterations bounds tool rounds for the whole run,
	// including nested agents. Nil uses 64.
	TotalMaxToolIterations *int `yaml:"total_max_tool_iterations"`
	// MaxToolCalls bounds tool-call attempts for the whole run, including
	// calls later denied or short-circuited. Nil, zero, and negative mean
	// unlimited.
	MaxToolCalls *int `yaml:"max_tool_calls"`
	// ToolDrainTimeout is how long a cancelled run waits for tool calls
	// to return before the session is marked poisoned. Zero uses 30s.
	// ForceReleaseSession is the explicit recovery.
	ToolDrainTimeout string `yaml:"tool_drain_timeout"`
	// EventHoldTimeout is how long a finished run waits for a consumer
	// who has not taken an event. The first take cancels that wait.
	// Zero uses 30s.
	EventHoldTimeout string `yaml:"event_hold_timeout"`
	// EventIdleTimeout is how long a finished run waits after the
	// consumer's last take before the pump keeps only the terminal event.
	// Zero uses 5m. A reader who keeps taking events is left alone.
	EventIdleTimeout string `yaml:"event_idle_timeout"`
}

// ApprovalConfig requires an approval for mutating tools when true.
type ApprovalConfig struct {
	Required bool `yaml:"required"`
}

// Int returns a pointer to n for an optional numeric limit.
func Int(n int) *int {
	v := n
	return &v
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
	if c.Workspace.MaxReadBytes == 0 {
		c.Workspace.MaxReadBytes = 1 << 20
	}
	if c.Workspace.MaxSearchHits == 0 {
		c.Workspace.MaxSearchHits = 100
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
	if c.Context.MaxLLMCalls == nil {
		c.Context.MaxLLMCalls = Int(128)
	}
	if c.Context.MaxToolIterations == nil {
		c.Context.MaxToolIterations = Int(64)
	}
	if c.Context.TotalMaxLLMCalls == nil {
		c.Context.TotalMaxLLMCalls = Int(128)
	}
	if c.Context.TotalMaxToolIterations == nil {
		c.Context.TotalMaxToolIterations = Int(64)
	}
	if c.Context.ToolDrainTimeout == "" {
		c.Context.ToolDrainTimeout = "30s"
	}
	if c.Context.EventHoldTimeout == "" {
		c.Context.EventHoldTimeout = "30s"
	}
	if c.Context.EventIdleTimeout == "" {
		c.Context.EventIdleTimeout = "5m"
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
