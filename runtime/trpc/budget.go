package trpcruntime

import (
	"context"
	"errors"

	"github.com/lookerjin/grape-core/agent"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// errRunBudget stops the callback when the run-level counter is exhausted.
// The terminal reason comes from runState, not from this text.
var errRunBudget = errors.New("run budget exceeded")

func invocationMeta(ctx context.Context) (id, name string) {
	inv, ok := trpcagent.InvocationFromContext(ctx)
	if !ok || inv == nil {
		return "", ""
	}
	return inv.InvocationID, inv.AgentName
}

// addLLM counts one model call that tRPC allowed into BeforeModel.
// The per-agent limit itself is tRPC's WithMaxLLMCalls. Call N+1 raises
// StopError before this callback, so the root count stays at the limit.
func (st *runState) addLLM(invocationID string) (rootN, total int) {
	if st == nil {
		return 0, 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.llmCalls++
	if invocationID != "" && invocationID == st.rootInv {
		st.rootLLM++
	}
	return st.rootLLM, st.llmCalls
}

// addToolRound counts one model turn that requested tools.
// AfterModel runs before tRPC's iteration check, so the exceeding round
// is visible here.
func (st *runState) addToolRound(invocationID string) (rootN, total int, isRoot bool) {
	if st == nil {
		return 0, 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toolRounds++
	isRoot = invocationID != "" && invocationID == st.rootInv
	if isRoot {
		st.rootTools++
	}
	return st.rootTools, st.toolRounds, isRoot
}

// reserveToolCall counts one attempt and reports whether it is inside the
// limit. The increment and the check share the run mutex, so parallel
// calls cannot all pass a limit of one.
func (st *runState) reserveToolCall(limit int) (allowed bool) {
	if st == nil {
		return true
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.toolCalls++
	if limit > 0 && st.toolCalls > limit {
		return false
	}
	return true
}

func (st *runState) runBudgetExhausted() bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.stopSet && st.stopReason == agent.ReasonBudgetExceeded && st.stopScope == agent.ScopeRun
}

func (st *runState) rootCounts() (llm, tools int) {
	if st == nil {
		return 0, 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.rootLLM, st.rootTools
}

func (st *runState) setStop(reason, scope, agentID string) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.stopSet {
		return
	}
	st.stopSet = true
	st.stopReason = reason
	st.stopScope = scope
	st.stopAgent = agentID
}

func (st *runState) stop() (reason, scope, agentID string, ok bool) {
	if st == nil {
		return "", "", "", false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.stopSet {
		return "", "", "", false
	}
	return st.stopReason, st.stopScope, st.stopAgent, true
}

func (rt *Runtime) consumeLLMCall(ctx context.Context) error {
	st := stateFrom(ctx)
	if rt == nil || st == nil {
		return nil
	}
	if st.runBudgetExhausted() {
		return trpcagent.NewStopError("run budget exceeded")
	}
	id, name := invocationMeta(ctx)
	_, total := st.addLLM(id)
	limit := positiveLimit(rt.cfg.Context.TotalMaxLLMCalls)
	if limit > 0 && total > limit {
		st.setStop(agent.ReasonBudgetExceeded, agent.ScopeRun, name)
		return errRunBudget
	}
	return nil
}

func (rt *Runtime) consumeToolCall(ctx context.Context) error {
	st := stateFrom(ctx)
	if rt == nil || st == nil {
		return nil
	}
	limit := positiveLimit(rt.cfg.Context.MaxToolCalls)
	if st.reserveToolCall(limit) {
		return nil
	}
	_, name := invocationMeta(ctx)
	st.setStop(agent.ReasonBudgetExceeded, agent.ScopeRun, name)
	// A plain callback error only rejects this call. StopError ends the
	// invocation, so the model is not called again.
	return trpcagent.NewStopError("run budget exceeded")
}

func (rt *Runtime) noteToolRound(ctx context.Context, resp *model.Response) error {
	if rt == nil || !responseRequestsTools(resp) {
		return nil
	}
	st := stateFrom(ctx)
	if st == nil {
		return nil
	}
	id, name := invocationMeta(ctx)
	rootN, total, isRoot := st.addToolRound(id)
	totalLimit := positiveLimit(rt.cfg.Context.TotalMaxToolIterations)
	if totalLimit > 0 && total > totalLimit {
		st.setStop(agent.ReasonBudgetExceeded, agent.ScopeRun, name)
		return errRunBudget
	}
	perAgent := positiveLimit(rt.cfg.Context.MaxToolIterations)
	if isRoot && perAgent > 0 && rootN > perAgent {
		st.setStop(agent.ReasonBudgetExceeded, agent.ScopeAgent, name)
	}
	return nil
}

func responseRequestsTools(resp *model.Response) bool {
	if resp == nil || resp.IsPartial {
		return false
	}
	for _, choice := range resp.Choices {
		if len(choice.Message.ToolCalls) > 0 || len(choice.Delta.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// observeLimit records why a root invocation stopped.
// tRPC's per-agent StopError becomes stop_agent_error and is then dropped
// from RunnerCompletion, so the run keeps the reason itself.
// Error.Type is compared. The message text is not.
func (rt *Runtime) observeLimit(st *runState, evt *event.Event) {
	if rt == nil || st == nil || evt == nil || evt.Response == nil || evt.Error == nil {
		return
	}
	// A tool or model error that does not end the run is recoverable.
	// Only an explicit error object can decide the result.
	if evt.Object != model.ObjectTypeError {
		return
	}
	root := st.rootInvocation()
	if root == "" || evt.InvocationID != root {
		return
	}
	switch evt.Error.Type {
	case trpcagent.ErrorTypeStopAgentError:
		limit := positiveLimit(rt.cfg.Context.MaxLLMCalls)
		llm, _ := st.rootCounts()
		if limit > 0 && llm >= limit {
			st.setStop(agent.ReasonBudgetExceeded, agent.ScopeAgent, evt.Author)
			return
		}
		st.setStop(agent.ReasonAgentStopped, agent.ScopeAgent, evt.Author)
	case model.ErrorTypeFlowError:
		_, tools := st.rootCounts()
		perAgent := positiveLimit(rt.cfg.Context.MaxToolIterations)
		if perAgent > 0 && tools > perAgent {
			st.setStop(agent.ReasonBudgetExceeded, agent.ScopeAgent, evt.Author)
			return
		}
		if evt.Done {
			// The completion event does not carry this error. The reason is
			// generic; the message text is not the classifier.
			st.setStop(agent.ReasonError, "", evt.Author)
		}
	}
}

func applyStop(st *runState, evt agent.Event) agent.Event {
	if st == nil || evt.Kind != agent.EventCompleted {
		return evt
	}
	reason, scope, agentID, ok := st.stop()
	if !ok {
		return evt
	}
	evt.Kind = agent.EventFailed
	if agentID != "" && evt.AgentID == "" {
		evt.AgentID = agentID
	}
	evt.Payload = agent.MustPayload(agent.TerminalPayload{
		Status:  string(agent.EventFailed),
		Reason:  reason,
		Scope:   scope,
		AgentID: agentID,
	})
	return evt
}
