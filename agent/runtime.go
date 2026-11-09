package agent

import (
	"context"
	"time"
)

// Mode selects how a run is assembled. Default is a single agent loop.
type Mode string

const (
	ModeDefault Mode = "default"
	ModePlan    Mode = "plan"
	ModeGoal    Mode = "goal"
)

// Permission is the workspace execution policy requested by the caller.
const (
	PermissionReadOnly         = "read_only"
	PermissionWorkspaceWrite   = "workspace_write"
	PermissionDangerFullAccess = "danger_full_access"
)

// RunRequest starts one turn. UserID and SessionID identify the session.
// RunID is assigned by the runtime and is not part of the request.
type RunRequest struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	Workspace string `json:"workspace,omitempty"`
	Message   string `json:"message"`
	Mode      Mode   `json:"mode,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

// RunHandle is the handle for an in-flight run.
//
// Events is closed after exactly one terminal event. Cancel aborts the run.
// Close drops any events still buffered and releases the pump. Call it when
// the consumer will not read Events. After a run finishes,
// context.event_hold_timeout (default 30s) bounds the wait for a consumer
// who has not taken an event. The first take cancels that wait, so a reader
// slower than the timeout still receives tool, permission, and error events.
// When the wait elapses with no take, the pump discards non-terminal events,
// leaves the terminal event, and exits. Runtime.Close releases every pump
// immediately. The context
// passed to Runtime.Start bounds the run; Cancel is an additional explicit
// abort. Implementations must not detach the run onto context.Background.
type RunHandle interface {
	ID() string
	Events() <-chan Event
	Cancel() error
	// Close releases the event pump. It is safe to call more than once.
	Close() error
	// Enqueue inserts a user message at the next tool boundary when the
	// underlying runner supports steering.
	Enqueue(message string) error
}

// Runtime starts runs and releases resources it owns.
type Runtime interface {
	Start(ctx context.Context, req RunRequest) (RunHandle, error)
	Close() error
}

// SessionInfo is a framework-free description of a stored session.
type SessionInfo struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	AppName   string    `json:"app_name,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// Task is a background task whose lifetime is owned by a TaskHost, not by
// the caller context that requested it.
type Task struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	UserID    string    `json:"user_id"`
	Status    string    `json:"status"`
	RunID     string    `json:"run_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
