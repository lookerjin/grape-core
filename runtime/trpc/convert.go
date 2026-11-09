package trpcruntime

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/redact"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// converter turns one tRPC event stream into grape-core events.
// Sequence numbers belong to this run only.
type converter struct {
	runID     string
	sessionID string
	seenTool  map[string]struct{}
	openTool  map[string]string
	maxField  int
	secrets   []string
	seq       uint64
}

func newConverter(runID, sessionID string, maxField int, secrets []string) *converter {
	if maxField <= 0 {
		maxField = 8192
	}
	return &converter{
		runID:     runID,
		sessionID: sessionID,
		seenTool:  map[string]struct{}{},
		openTool:  map[string]string{},
		maxField:  maxField,
		secrets:   secrets,
	}
}

func (c *converter) convert(evt *event.Event) []agent.Event {
	if evt == nil || evt.Response == nil {
		return nil
	}
	if evt.IsRunnerCompletion() {
		out := c.closeOpenTools(evt)
		out = append(out, c.terminal(evt))
		return out
	}
	var out []agent.Event
	if evt.Object == model.ObjectTypeToolResponse {
		if evt.IsPartial {
			return nil
		}
		return c.nonEmpty(c.toolEnds(evt))
	}
	if evt.IsError() {
		out = append(out, c.errorEvent(evt))
	}
	out = append(out, c.toolStarts(evt)...)
	if text, partial, ok := textOf(evt); ok {
		kind := agent.EventText
		if partial {
			kind = agent.EventTextDelta
		}
		out = append(out, c.base(evt, kind, agent.MustPayload(agent.TextPayload{
			Text:    c.clean(text),
			Partial: partial,
		})))
	}
	if len(out) == 0 && !evt.IsPartial && evt.Object != "" &&
		!strings.HasPrefix(evt.Object, "preprocessing.") &&
		!strings.HasPrefix(evt.Object, "chat.completion") {
		out = append(out, c.base(evt, agent.EventRaw, agent.MustPayload(agent.RawPayload{
			Object: evt.Object,
			Author: evt.Author,
		})))
	}
	return c.nonEmpty(out)
}

func (c *converter) nonEmpty(in []agent.Event) []agent.Event {
	if len(in) == 0 {
		return nil
	}
	return in
}

func (c *converter) terminal(evt *event.Event) agent.Event {
	status := agent.EventCompleted
	msg := ""
	if evt.RunOutcome != nil {
		switch evt.RunOutcome.Status {
		case event.RunOutcomeStatusCancelled:
			status = agent.EventCancelled
		case event.RunOutcomeStatusTimedOut:
			status = agent.EventFailed
			msg = "timed out"
		}
	}
	if evt.Error != nil && status == agent.EventCompleted {
		status = agent.EventFailed
		msg = evt.Error.Message
		if evt.Error.Type == model.ErrorTypeCancelled {
			status = agent.EventCancelled
		}
	}
	return c.base(evt, status, agent.MustPayload(agent.TerminalPayload{
		Status: string(status),
		Error:  c.clean(msg),
	}))
}

func (c *converter) errorEvent(evt *event.Event) agent.Event {
	msg := ""
	typ := ""
	if evt.Error != nil {
		msg = evt.Error.Message
		typ = evt.Error.Type
	}
	return c.base(evt, agent.EventError, agent.MustPayload(agent.ErrorPayload{
		Message: c.clean(msg),
		Type:    typ,
	}))
}

func (c *converter) toolStarts(evt *event.Event) []agent.Event {
	var calls []model.ToolCall
	for _, choice := range evt.Choices {
		calls = append(calls, choice.Message.ToolCalls...)
		calls = append(calls, choice.Delta.ToolCalls...)
	}
	var out []agent.Event
	for _, call := range calls {
		key := call.ID
		if key == "" {
			key = call.Function.Name + ":" + string(call.Function.Arguments)
		}
		if key == "" {
			continue
		}
		if _, ok := c.seenTool[key]; ok {
			continue
		}
		c.seenTool[key] = struct{}{}
		if c.openTool == nil {
			c.openTool = map[string]string{}
		}
		c.openTool[key] = call.Function.Name
		args := c.clipRaw(call.Function.Arguments)
		ev := c.base(evt, agent.EventToolStart, agent.MustPayload(agent.ToolPayload{
			Name:      call.Function.Name,
			CallID:    call.ID,
			Arguments: args,
		}))
		ev.ToolCallID = call.ID
		out = append(out, ev)
	}
	return out
}

func (c *converter) toolEnds(evt *event.Event) []agent.Event {
	if len(evt.Choices) == 0 {
		return []agent.Event{c.toolEndOne(evt, "", "", "")}
	}
	var out []agent.Event
	seen := map[string]struct{}{}
	for _, choice := range evt.Choices {
		name := choice.Message.ToolName
		id := choice.Message.ToolID
		content := choice.Message.Content
		if id == "" {
			id = choice.Delta.ToolID
		}
		if name == "" {
			name = choice.Delta.ToolName
		}
		if content == "" {
			content = choice.Delta.Content
		}
		key := id
		if key == "" {
			key = name
		}
		if key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		out = append(out, c.toolEndOne(evt, name, id, content))
	}
	return out
}

func (c *converter) toolEndOne(evt *event.Event, name, id, content string) agent.Event {
	key := id
	if key == "" {
		key = name
	}
	if key != "" {
		delete(c.openTool, key)
	}
	cleaned, truncated := c.clip(content)
	result := json.RawMessage(strconvQuote(cleaned))
	ev := c.base(evt, agent.EventToolEnd, agent.MustPayload(agent.ToolPayload{
		Name:      name,
		CallID:    id,
		Result:    result,
		Truncated: truncated,
	}))
	ev.ToolCallID = id
	return ev
}

func (c *converter) closeOpenTools(evt *event.Event) []agent.Event {
	if len(c.openTool) == 0 {
		return nil
	}
	ids := make([]string, 0, len(c.openTool))
	for id := range c.openTool {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []agent.Event
	for _, id := range ids {
		name := c.openTool[id]
		out = append(out, c.toolEndOne(evt, name, id, ""))
	}
	return out
}

func strconvQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func (c *converter) stamp(evt agent.Event) agent.Event {
	c.seq++
	evt.Sequence = c.seq
	if evt.Time.IsZero() {
		evt.Time = time.Now().UTC()
	}
	return evt
}

func (c *converter) base(evt *event.Event, kind agent.EventKind, payload json.RawMessage) agent.Event {
	c.seq++
	ts := evt.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	ev := agent.Event{
		RunID:     c.runID,
		SessionID: c.sessionID,
		AgentID:   evt.Author,
		TraceID:   evt.InvocationID,
		Kind:      kind,
		Sequence:  c.seq,
		Time:      ts,
		Payload:   payload,
	}
	if evt.RequestID != "" {
		ev.RunID = evt.RequestID
	}
	if evt.Usage != nil && (evt.Usage.TotalTokens > 0 || evt.Usage.PromptTokens > 0 || evt.Usage.CompletionTokens > 0) {
		ev.Usage = &agent.Usage{
			PromptTokens:     evt.Usage.PromptTokens,
			CompletionTokens: evt.Usage.CompletionTokens,
			TotalTokens:      evt.Usage.TotalTokens,
		}
	}
	return ev
}

func textOf(evt *event.Event) (string, bool, bool) {
	if len(evt.Choices) == 0 {
		return "", false, false
	}
	choice := evt.Choices[0]
	if evt.IsPartial {
		if choice.Delta.Content == "" {
			return "", false, false
		}
		return choice.Delta.Content, true, true
	}
	if choice.Message.Content == "" {
		return "", false, false
	}
	if len(choice.Message.ToolCalls) > 0 {
		return "", false, false
	}
	return choice.Message.Content, false, true
}

func (c *converter) clean(s string) string {
	return redact.Text(s, c.secrets)
}

func (c *converter) clip(s string) (string, bool) {
	s = c.clean(s)
	return redact.Truncate(s, c.maxField)
}

func (c *converter) clipRaw(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	s, truncated := c.clip(string(raw))
	if !truncated && json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}
