package trpcruntime

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/lookerjin/grape-core/agent"
	native "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

type childCall struct {
	info   agent.SubagentCall
	cancel context.CancelFunc
}
type observedChild struct {
	native.Agent
	model string
}

// Observe the native output until closure, including when cancellation makes
// RunWithPlugins skip its AfterAgent callback. The AgentTool still owns execution.
func (a observedChild) Run(ctx context.Context, inv *native.Invocation) (<-chan *event.Event, error) {
	st := stateFrom(ctx)
	if st == nil {
		return a.Agent.Run(ctx, inv)
	}
	child, cancel := context.WithCancel(ctx)
	call := &childCall{cancel: cancel, info: agent.SubagentCall{InvocationID: inv.InvocationID, AgentID: a.Info().Name, Model: a.model, Status: "running"}}
	if m := inv.RunOptions.Model; m != nil {
		call.info.Model = m.Info().Name
	}
	if p := inv.GetParentInvocation(); p != nil {
		call.info.ParentInvocationID = p.InvocationID
	}
	if inv.ParentMetadata != nil {
		call.info.ToolCallID = inv.ParentMetadata.TriggerID
	}
	st.mu.Lock()
	if st.children == nil {
		st.children = map[string]*childCall{}
	}
	st.children[inv.InvocationID] = call
	st.mu.Unlock()
	finish := func(failed bool) {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case child.Err() != nil:
			call.info.Status = "cancelled"
		case failed:
			call.info.Status = "failed"
		default:
			call.info.Status = "completed"
		}
		call.cancel = nil
		cancel()
	}
	src, err := a.Agent.Run(child, inv)
	if err != nil {
		finish(true)
		return nil, err
	}
	out := make(chan *event.Event)
	go func() {
		defer close(out)
		failed := false
		for evt := range src {
			if evt != nil && evt.Response != nil {
				st.mu.Lock()
				call.info.Events++
				call.info.LastObject = evt.Object
				st.mu.Unlock()
				if evt.Done {
					failed = failed || evt.Error != nil
				}
			}
			select {
			case out <- evt:
			case <-child.Done():
			}
		}
		finish(failed)
	}()
	return out, nil
}

func (h *runHandle) Subagents() []agent.SubagentCall {
	st := h.state
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]agent.SubagentCall, 0, len(st.children))
	for _, c := range st.children {
		out = append(out, c.info)
	}
	slices.SortFunc(out, func(a, b agent.SubagentCall) int { return cmp.Compare(a.InvocationID, b.InvocationID) })
	return out
}

func (h *runHandle) CancelSubagent(id string) error {
	st := h.state
	st.mu.Lock()
	defer st.mu.Unlock()
	c := st.children[id]
	if c == nil || c.cancel == nil {
		return fmt.Errorf("no active child invocation %q", id)
	}
	c.info.Status = "cancelling"
	c.cancel()
	return nil
}

func (h *runHandle) ContextStatus() (agent.ContextStatus, bool) {
	st := h.state
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.projection, st.hasProjection
}

func (st *runState) noteProjection(ctx context.Context, m model.Info, req *model.Request) {
	if st == nil || req == nil {
		return
	}
	id, _ := invocationMeta(ctx)
	if id == "" || id != st.rootInvocation() {
		return
	}
	status := agent.ContextStatus{Model: m.Name, ContextWindow: m.ContextWindow, Messages: len(req.Messages), Tools: len(req.Tools), CompactionEnabled: st.compaction}
	for _, msg := range req.Messages {
		n := len(msg.Content)
		for _, p := range msg.ContentParts {
			if p.Text != nil {
				n += len(*p.Text)
			}
		}
		switch msg.Role {
		case model.RoleSystem:
			status.SystemBytes += n
		case model.RoleUser:
			status.UserBytes += n
		case model.RoleAssistant:
			status.AssistantBytes += n
		case model.RoleTool:
			status.ToolResultBytes += n
		}
	}
	st.mu.Lock()
	st.projection = status
	st.hasProjection = true
	st.mu.Unlock()
}

var _ agent.RunInspector = (*runHandle)(nil)
