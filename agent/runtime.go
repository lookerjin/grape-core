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

// ReasoningOptions selects one provider-native control for this turn. Zero uses provider defaults.
type ReasoningOptions struct {
	Effort       string `json:"effort,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// RunRequest starts one turn. UserID and SessionID identify the session.
// RunID is assigned by the runtime and is not part of the request.
type RunRequest struct {
	Reasoning *ReasoningOptions `json:"reasoning,omitempty"`
	ModelRef  *ModelRef         `json:"model_ref,omitempty"`
	// Permission may narrow the runtime ceiling. RequireApproval adds an approval gate.
	Permission      string        `json:"permission,omitempty"`
	RequireApproval bool          `json:"require_approval,omitempty"`
	ModelName       string        `json:"model_name,omitempty"`
	Attachments     []Attachment  `json:"attachments,omitempty"`
	Output          *OutputSchema `json:"output,omitempty"`
	UserID          string        `json:"user_id"`
	SessionID       string        `json:"session_id"`
	Workspace       string        `json:"workspace,omitempty"`
	Message         string        `json:"message"`
	Mode            Mode          `json:"mode,omitempty"`
	AgentID         string        `json:"agent_id,omitempty"`
}

// RunHandle is the handle for an in-flight run.
//
// A consumer that reads Events until it closes sees exactly one terminal
// event. Close, or the hold and idle deadlines, may close Events without
// that event. Status reports execution and stream delivery separately. Cancel
// aborts the run. The context passed to Runtime.Start bounds the run.
// Implementations must not detach the run onto context.Background.
type RunHandle interface {
	ID() string
	Events() <-chan Event
	// Status reports the run result. It is pending until the run stops.
	// Close can leave status pending with delivery abandoned until the runner stops.
	Status() RunStatus
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

// SessionRelease names the session to unlock and whether a tool may still
// be running. AcceptConcurrentTools is not safe recovery: the next run can
// overlap a tool or child that has not finished.
type SessionRelease struct {
	// RunID, when supplied, must match the owner of the draining session.
	RunID                 string
	UserID                string
	AcceptConcurrentTools bool
}

// TaskInterrupted is the error stored when a task was still running at
// process restart. Compare this value; do not match the text by hand.
const TaskInterrupted = "interrupted by previous runtime restart"

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

// SessionHistoryReader is an optional read-only capability of a Runtime.
// eventLimit bounds the number of recent persisted events before projection.
// Zero uses the session service's configured limit.
type SessionHistoryReader interface {
	SessionHistory(ctx context.Context, userID, sessionID string, eventLimit int) ([]HistoryMessage, error)
}

// HistoryMessage is a persisted conversation message in chronological order.
type HistoryMessage struct {
	Attachments []AttachmentInfo  `json:"attachments,omitempty"`
	EventID     string            `json:"event_id"`
	Time        time.Time         `json:"time"`
	Author      string            `json:"author,omitempty"`
	Role        string            `json:"role"`
	Text        string            `json:"text,omitempty"`
	ToolID      string            `json:"tool_id,omitempty"`
	ToolName    string            `json:"tool_name,omitempty"`
	ToolCalls   []HistoryToolCall `json:"tool_calls,omitempty"`
}

type HistoryToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ModelInfo describes a configured model and the request features its adapter exposes.
type ModelInfo struct {
	ReasoningEffortSupport, ThinkingBudgetSupport string
	Name, Provider                                string
	ContextWindow                                 int
	KnownAdapter, Multipart, JSONSchema           bool
}

// ModelRef identifies a model on one specific connection.
type ModelRef struct {
	ConnectionID string `json:"connection_id"`
	ModelID      string `json:"model_id"`
}

// SessionBaselineID identifies the persisted state before a new session's first turn.
// Pass it to the existing fork, preview and rewind APIs like a completed Run ID.
const SessionBaselineID = "baseline"

// TurnCheckpoint identifies a conversation boundary. Baseline is not a completed Run.
// Forks carry their source.
type TurnCheckpoint struct {
	Baseline      bool   `json:"baseline,omitempty"`
	RunID         string `json:"run_id"`
	ParentSession string `json:"parent_session,omitempty"`
	ParentRun     string `json:"parent_run,omitempty"`
}

// SideAnswer is independent of the main Run and its accounting.
type SideAnswer struct {
	Checkpoint string
	Text       string
	Usage      *Usage
}

// FileRestorePlan is bound to the session, checkpoint, change history and observed file versions.
type FileRestorePlan struct {
	Digest string
	// TrackingScope names the write sources represented by this plan.
	TrackingScope []string
	Files         []FileRestoreItem
}
type FileRestoreItem struct{ Root, Path, Expected, Actual, Target, Conflict, Action string }

// BackupInfo identifies a durably prepared backup and its last confirmed state.
type BackupInfo struct{ Root, Path, Status string }

// RecoveryRecord stores the independent result of a context or workspace recovery attempt.
type RecoveryRecord struct {
	ID, RunID, Kind, Status, Detail string
	CreatedAt                       time.Time
}

// RecoveryLimits describes fixed storage bounds. Session byte caps count encoded database blobs.
type RecoveryLimits struct{ FileBytes, SessionChangeBytes, SessionBackupBytes int }
