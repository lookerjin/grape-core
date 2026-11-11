package trpcruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/redact"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// AskSide makes exactly one non-streaming, tool-free model request against the
// latest committed conversation. No new session, tools or agent loop are created.
func (rt *Runtime) AskSide(ctx context.Context, user, id, question string, ref *agent.ModelRef) (agent.SideAnswer, error) {
	var answer agent.SideAnswer
	if err := rt.checkpointScope(user, id); err != nil {
		return answer, err
	}
	if strings.TrimSpace(question) == "" {
		return answer, agent.Invalid("question", errors.New("empty question"))
	}
	rt.mu.Lock()
	if rt.closed {
		rt.mu.Unlock()
		return answer, agent.ErrSession
	}
	rt.starting.Add(1)
	rt.mu.Unlock()
	defer rt.starting.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(rt.lifetime, cancel)
	defer stop()
	tx, err := rt.turnDB.BeginTx(ctx, nil)
	if err != nil {
		return answer, err
	}
	defer tx.Rollback()
	b, err := rt.boundary(ctx, tx, user, id, "")
	if err != nil {
		return answer, err
	}
	// Keep provider attachments and tool call pairs intact. Exclude private child branches.
	rows, err := tx.QueryContext(ctx, "SELECT event FROM session_events WHERE "+sessionScope+" AND id<=? ORDER BY id", rt.appName, user, id, b.eventID)
	if err != nil {
		return answer, err
	}
	var messages []model.Message
	var size int
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return answer, err
		}
		var e event.Event
		if err = json.Unmarshal(raw, &e); err != nil {
			rows.Close()
			return answer, err
		}
		if e.Response == nil || e.IsPartial || e.Branch != rootAgentName {
			continue
		}
		for _, choice := range e.Choices {
			if choice.Message.Role == "" {
				continue
			}
			messages = append(messages, choice.Message)
			size += len(raw)
			if size > 16<<20 {
				rows.Close()
				return answer, fmt.Errorf("side question context exceeds 16 MiB")
			}
		}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return answer, err
	}
	rows.Close()
	if err = tx.Commit(); err != nil {
		return answer, err
	}
	if rt.registry != nil && ref == nil {
		r, e := rt.registry.Default(ctx, rt.registry.Scope())
		if e != nil {
			return answer, e
		}
		ref = &r
	}
	m, _, key, err := rt.selectModel(ctx, agent.RunRequest{UserID: user, ModelRef: ref})
	if err != nil {
		return answer, err
	}
	if m == nil {
		if rt.fixed != nil {
			return answer, agent.Invalid("model", errors.New("side questions require a model"))
		}
		m = rt.model
	}
	gen := generationConfig(rt.cfg)
	gen.Stream = false
	source, err := m.GenerateContent(ctx, &model.Request{Messages: append(messages, model.NewUserMessage(question)), GenerationConfig: gen})
	if err != nil {
		return answer, err
	}
	answer.Checkpoint = b.RunID
	for {
		select {
		case <-ctx.Done():
			return agent.SideAnswer{}, ctx.Err()
		case r, ok := <-source:
			if !ok {
				if ctx.Err() != nil {
					return agent.SideAnswer{}, ctx.Err()
				}
				if answer.Text == "" {
					return agent.SideAnswer{}, agent.ErrModel
				}
				answer.Text = redact.Text(answer.Text, append(append([]string(nil), rt.secretSnapshot()...), key))
				return answer, nil
			}
			if r == nil {
				continue
			}
			if r.Error != nil {
				return agent.SideAnswer{}, agent.ErrModel
			}
			if u := publicUsage(r.Usage); u != nil {
				answer.Usage = u
			}
			for _, ch := range r.Choices {
				if len(ch.Message.ToolCalls) > 0 {
					return agent.SideAnswer{}, agent.ErrPermissionDenied
				}
				answer.Text += ch.Message.Content
			}
		}
	}
}
