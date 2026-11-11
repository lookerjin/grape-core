package trpcruntime

import (
	"context"
	"fmt"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/redact"
	"trpc.group/trpc-go/trpc-agent-go/session"
)

var _ agent.SessionHistoryReader = (*Runtime)(nil)

// SessionHistory reads tRPC's persisted conversation, scoped to this app and user.
func (rt *Runtime) SessionHistory(ctx context.Context, userID, sessionID string, eventLimit int) ([]agent.HistoryMessage, error) {
	if eventLimit < 0 {
		return nil, agent.Invalid("eventLimit", fmt.Errorf("must be non-negative"))
	}
	key := session.Key{AppName: rt.appName, UserID: userID, SessionID: sessionID}
	sess, err := rt.sessions.GetSession(ctx, key, session.WithEventNum(eventLimit))
	if err != nil {
		return nil, agent.Wrap(agent.ErrSession, "history", err)
	}
	if sess == nil {
		return nil, agent.Wrap(agent.ErrSession, "history", fmt.Errorf("session not found"))
	}
	out := []agent.HistoryMessage{}
	clean := func(s string) string { return redact.Text(s, rt.secrets) }
	for _, evt := range sess.GetEvents() {
		if evt.Response == nil || evt.IsPartial || evt.IsRunnerCompletion() {
			continue
		}
		for _, choice := range evt.Choices {
			msg := choice.Message
			if msg.Role == "" {
				continue
			}
			var text strings.Builder
			text.WriteString(msg.Content)
			for _, part := range msg.ContentParts {
				if part.Text != nil {
					text.WriteString(*part.Text)
				}
			}
			row := agent.HistoryMessage{EventID: evt.ID, Time: evt.Timestamp, Author: clean(evt.Author), Role: string(msg.Role), Text: clean(text.String()), ToolID: clean(msg.ToolID), ToolName: clean(msg.ToolName)}
			for _, call := range msg.ToolCalls {
				row.ToolCalls = append(row.ToolCalls, agent.HistoryToolCall{ID: clean(call.ID), Name: clean(call.Function.Name), Arguments: clean(string(call.Function.Arguments))})
			}
			out = append(out, row)
		}
	}
	return out, nil
}
