package trpcruntime

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/redact"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// converter turns one tRPC event stream into grape-core events.
// Sequence numbers belong to this run only.
type converter struct {
	schema       *jsonschema.Schema
	outputAuthor string
	validOutput  bool
	runID        string
	sessionID    string
	seenTool     map[string]struct{}
	openTool     map[string]string
	closeStatus  string
	failedToolID string
	toolError    string
	maxField     int
	secrets      []string
	seq          uint64
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
	if evt.StructuredOutput != nil && c.schema != nil && evt.Author == c.outputAuthor {
		c.validOutput = c.schema.Validate(evt.StructuredOutput) == nil
		if c.validOutput {
			raw, err := json.Marshal(evt.StructuredOutput)
			if err == nil && !c.containsJSONSecret(raw) {
				return []agent.Event{c.base(evt, agent.EventStructured, raw)}
			}
			c.validOutput = false
		}
	}
	if evt.IsRunnerCompletion() {
		out := c.closeOpenTools(evt, c.closeStatus)
		out = append(out, c.terminal(evt))
		return out
	}
	if evt.Error != nil && evt.Done && len(c.openTool) == 1 {
		// The runner can terminate before a tool response is emitted. Attribute
		// that failure only when the outstanding call is unambiguous.
		for id := range c.openTool {
			c.failedToolID = id
		}
		c.toolError = c.clean(evt.Error.Message)
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

// JSON may escape a credential (for example '<' or a quotation mark).
func (c *converter) containsJSONSecret(raw []byte) bool {
	for _, secret := range c.secrets {
		if len(secret) < 4 {
			continue
		}
		encoded, _ := json.Marshal(secret)
		if bytes.Contains(raw, encoded[1:len(encoded)-1]) {
			return true
		}
	}
	return false
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
		id := call.ID
		if id != "" {
			if _, ok := c.seenTool[id]; ok {
				continue
			}
			c.seenTool[id] = struct{}{}
		}
		if c.openTool == nil {
			c.openTool = map[string]string{}
		}
		c.openTool[id] = call.Function.Name
		args := c.clipRaw(call.Function.Arguments)
		ev := c.base(evt, agent.EventToolStart, agent.MustPayload(agent.ToolPayload{
			Name:      call.Function.Name,
			CallID:    id,
			Arguments: args,
		}))
		ev.ToolCallID = id
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
	if id != "" {
		delete(c.openTool, id)
	}
	errField := ""
	if id != "" && id == c.failedToolID {
		errField = c.toolError
		if content == "" {
			content = errField
		}
		c.failedToolID, c.toolError = "", ""
	}
	cleaned, truncated := c.clip(content)
	result := json.RawMessage(strconvQuote(cleaned))
	ev := c.base(evt, agent.EventToolEnd, agent.MustPayload(agent.ToolPayload{
		Name:      name,
		CallID:    id,
		Result:    result,
		Error:     errField,
		Truncated: truncated,
	}))
	ev.ToolCallID = id
	return ev
}

func (c *converter) closeOpenTools(evt *event.Event, status string) []agent.Event {
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
		ev := c.toolEndOne(evt, name, id, "")
		if status != "" {
			ev = withToolStatus(ev, status)
		}
		out = append(out, ev)
	}
	return out
}

func withToolStatus(evt agent.Event, status string) agent.Event {
	var body agent.ToolPayload
	_ = json.Unmarshal(evt.Payload, &body)
	body.Error = status
	if len(body.Result) == 0 || string(body.Result) == `""` {
		body.Result = json.RawMessage(strconvQuote(status))
	}
	evt.Payload = agent.MustPayload(body)
	return evt
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
	var ts time.Time
	author := ""
	trace := ""
	requestID := ""
	var usage *model.Usage
	if evt != nil {
		ts = evt.Timestamp
		author = evt.Author
		trace = evt.InvocationID
		requestID = evt.RequestID
		usage = evt.Usage
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	ev := agent.Event{
		RunID:     c.runID,
		SessionID: c.sessionID,
		AgentID:   author,
		TraceID:   trace,
		Kind:      kind,
		Sequence:  c.seq,
		Time:      ts,
		Payload:   payload,
	}
	if requestID != "" {
		ev.RunID = requestID
	}
	ev.Usage = publicUsage(usage)
	return ev
}

func publicUsage(u *model.Usage) *agent.Usage {
	if u == nil {
		return nil
	}
	read := u.PromptTokensDetails.CacheReadTokens
	if read == 0 {
		read = u.PromptTokensDetails.CachedTokens
	}
	write := u.PromptTokensDetails.CacheCreationTokens
	reason := u.CompletionTokensDetails.ReasoningTokens
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 && read == 0 && write == 0 && reason == 0 {
		return nil
	}
	out := &agent.Usage{Source: "provider", PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens}
	if read > 0 {
		out.CacheReadTokens = &read
	}
	if write > 0 {
		out.CacheWriteTokens = &write
	}
	if reason > 0 {
		out.ReasoningTokens = &reason
	}
	return out
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
