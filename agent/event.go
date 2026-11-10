package agent

import (
	"encoding/json"
	"time"
)

// EventKind is a product-stable event class. Framework-specific events that
// do not match a class are delivered as [EventRaw].
type EventKind string

const (
	EventTextDelta         EventKind = "text_delta"
	EventText              EventKind = "text"
	EventToolStart         EventKind = "tool_start"
	EventToolEnd           EventKind = "tool_end"
	EventPermissionRequest EventKind = "permission_request"
	EventArtifact          EventKind = "artifact"
	EventError             EventKind = "error"
	EventCompleted         EventKind = "completed"
	EventFailed            EventKind = "failed"
	EventCancelled         EventKind = "cancelled"
	EventRaw               EventKind = "raw"
)

// Usage is optional token accounting copied from the model response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

// Event is one item in a run stream.
//
// Sequence is a monotonic counter for this process-local run only. It does
// not mean the event was persisted or can be replayed after a disconnect.
type Event struct {
	RunID      string          `json:"run_id"`
	SessionID  string          `json:"session_id"`
	AgentID    string          `json:"agent_id,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	TraceID    string          `json:"trace_id,omitempty"`
	Kind       EventKind       `json:"kind"`
	Sequence   uint64          `json:"sequence"`
	Time       time.Time       `json:"time"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
}

// Terminal reports whether the event ends the run.
func (e Event) Terminal() bool {
	switch e.Kind {
	case EventCompleted, EventFailed, EventCancelled:
		return true
	default:
		return false
	}
}

// TextPayload is the body of text and text_delta events.
type TextPayload struct {
	Text    string `json:"text"`
	Partial bool   `json:"partial,omitempty"`
}

// ToolPayload describes a tool call or its result.
//
// Result and Arguments may be truncated. Truncated is set when the runtime
// dropped bytes to keep the stream small.
type ToolPayload struct {
	Name      string          `json:"name"`
	CallID    string          `json:"call_id,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

// TerminalPayload is the body of a terminal event.
// Terminal reason and scope values carried on a failed run.
const (
	ReasonBudgetExceeded = "budget_exceeded"
	ReasonAgentStopped   = "agent_stopped"
	ReasonError          = "error"
	ScopeRun             = "run"
	ScopeAgent           = "agent"
	// StatusPending is Status before the run has stopped.
	StatusPending = "pending"
	// CauseUser is a cancel requested by Cancel or the Start context.
	CauseUser = "user"
	// CauseDelivery means Close, hold, or idle closed the stream before a
	// result existed. It is not a result the run itself produced.
	CauseDelivery = "delivery"
	// DeliveryOpen means the stream has not closed.
	DeliveryOpen = "open"
	// DeliveryDelivered means the consumer was sent the terminal event.
	DeliveryDelivered = "delivered"
	// DeliveryAbandoned means Close, hold, or idle closed the stream
	// without delivering the terminal event.
	DeliveryAbandoned = "abandoned"
)

// RunStatus is the run result, separate from whether the stream delivered it.
// Before the run stops, Status is StatusPending and Delivery is DeliveryOpen.
// Cause is CauseUser for a caller cancel and CauseDelivery when the stream
// was closed before a result existed. Delivery records open, delivered, or
// abandoned. A result that already exists is kept when the stream is abandoned.
type RunStatus struct {
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Scope    string `json:"scope,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	Cause    string `json:"cause,omitempty"`
	Delivery string `json:"delivery,omitempty"`
}

type TerminalPayload struct {
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Scope   string `json:"scope,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
}

// ErrorPayload is the body of an error event.
type ErrorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

// PermissionPayload is the body of a permission_request event.
type PermissionPayload struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail,omitempty"`
	Path    string `json:"path,omitempty"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// RawPayload carries an unmatched framework event.
type RawPayload struct {
	Object string `json:"object,omitempty"`
	Author string `json:"author,omitempty"`
}

// MustPayload marshals v or returns an empty JSON object.
func MustPayload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return b
}
