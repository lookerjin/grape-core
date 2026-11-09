package agent

import "context"

// ApprovalRequest asks a product to allow or deny one side effect.
// The SDK never reads stdin. The product supplies a Decider.
type ApprovalRequest struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail,omitempty"`
	Path      string `json:"path,omitempty"`
}

// Decision is the product's answer to an ApprovalRequest.
type Decision struct {
	ID     string `json:"id"`
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// Decider resolves an approval. A nil Decider denies every request that
// requires approval.
type Decider interface {
	Decide(ctx context.Context, req ApprovalRequest) (Decision, error)
}

// DeciderFunc adapts a function to Decider.
type DeciderFunc func(context.Context, ApprovalRequest) (Decision, error)

// Decide implements Decider.
func (f DeciderFunc) Decide(ctx context.Context, req ApprovalRequest) (Decision, error) {
	return f(ctx, req)
}
