package agent

// RunInspector is an optional capability of a live RunHandle.
type RunInspector interface {
	Subagents() []SubagentCall
	CancelSubagent(invocationID string) error
	ContextStatus() (ContextStatus, bool)
}

// SubagentCall describes the lifetime of one Core-built child invocation.
type SubagentCall struct {
	InvocationID, ParentInvocationID, ToolCallID, AgentID, Model, Status string
	Events                                                               int
	LastObject                                                           string
}

// ContextStatus measures the last root request after native history projection.
// Byte counts cover message text and text parts; attachments and tool schemas are excluded.
type ContextStatus struct {
	Model                                                   string
	ContextWindow, Messages, Tools                          int
	SystemBytes, UserBytes, AssistantBytes, ToolResultBytes int
	CompactionEnabled                                       bool
}
