package sandbox

import (
	"context"
	"time"
)

// Mode is the isolation mode requested by configuration.
const (
	ModeManaged          = "managed"
	ModeDangerFullAccess = "danger_full_access"
	ModeExternal         = "external"
)

// Isolation values reported on a finished command.
const (
	IsolationManaged  = "managed"
	IsolationNone     = "none"
	IsolationExternal = "external"
)

// Capabilities is the result of a backend preflight.
type Capabilities struct {
	Platform             string   `json:"platform"`
	ManagedIsolation     bool     `json:"managed_isolation"`
	FilesystemIsolation  bool     `json:"filesystem_isolation"`
	NetworkIsolation     bool     `json:"network_isolation"`
	PTY                  bool     `json:"pty"`
	Interactive          bool     `json:"interactive"`
	SensitivePathsMasked []string `json:"sensitive_paths_masked,omitempty"`
	Reason               string   `json:"reason,omitempty"`
}

// Request is one command. Argv is executed directly. It is not a shell string.
type Request struct {
	Workspace string
	Argv      []string
	Timeout   time.Duration
	Env       map[string]string
}

// Result is the outcome of a command that was actually started.
// Isolation is the level that was enforced, not the level that was requested.
type Result struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Isolation string `json:"isolation"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

// Executor runs a command under a resolved policy.
type Executor interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Execute(ctx context.Context, req Request) (Result, error)
}

// Backend is the stable extension point for a future Windows native sandbox
// or another external isolator. A backend is usable only after Healthy
// succeeds. A configuration flag alone is not a health check.
type Backend interface {
	Name() string
	Healthy(ctx context.Context) error
	Execute(ctx context.Context, req Request) (Result, error)
}
