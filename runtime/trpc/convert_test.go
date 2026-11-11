package trpcruntime

import (
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestConvertTextToolAndTerminal(t *testing.T) {
	c := newConverter("run", "sess", 32, []string{"sk-secret"})
	partial := &event.Event{
		Response: &model.Response{
			Object:    model.ObjectTypeChatCompletionChunk,
			IsPartial: true,
			Choices: []model.Choice{{
				Delta: model.Message{Content: "hel"},
			}},
		},
		Author:    "grape",
		RequestID: "run",
		Timestamp: time.Unix(10, 0).UTC(),
	}
	text := c.convert(partial)
	if len(text) != 1 || text[0].Kind != agent.EventTextDelta || text[0].Sequence != 1 {
		t.Fatalf("delta %#v", text)
	}
	final := &event.Event{
		Response: &model.Response{
			Object: model.ObjectTypeChatCompletion,
			Choices: []model.Choice{{
				Message: model.Message{
					Role:    model.RoleAssistant,
					Content: "hello sk-secret",
					ToolCalls: []model.ToolCall{{
						ID:   "c1",
						Type: "function",
						Function: model.FunctionDefinitionParam{
							Name:      "echo",
							Arguments: []byte(`{"text":"sk-secret"}`),
						},
					}},
				},
			}},
			Usage: &model.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
		},
		InvocationID: "inv",
		RequestID:    "run",
	}
	got := c.convert(final)
	if len(got) != 1 || got[0].Kind != agent.EventToolStart {
		t.Fatalf("tool %#v", got)
	}
	if got[0].Usage == nil || got[0].Usage.TotalTokens != 7 {
		t.Fatalf("usage %#v", got[0].Usage)
	}
	again := c.convert(final)
	if len(again) != 0 {
		t.Fatalf("duplicate tool call %#v", again)
	}
	toolEvt := &event.Event{
		Response: &model.Response{
			Object: model.ObjectTypeToolResponse,
			Choices: []model.Choice{{
				Message: model.Message{ToolName: "echo", ToolID: "c1", Content: "sk-secret-and-more-than-thirty-two-characters"},
			}},
		},
		RequestID: "run",
	}
	end := c.convert(toolEvt)
	if len(end) != 1 || end[0].Kind != agent.EventToolEnd {
		t.Fatalf("end %#v", end)
	}
	if string(end[0].Payload) == "" || contains(string(end[0].Payload), "sk-secret") {
		t.Fatalf("payload %s", end[0].Payload)
	}
	done := &event.Event{
		Response: &model.Response{
			Object: model.ObjectTypeRunnerCompletion,
			Done:   true,
		},
		RequestID: "run",
	}
	term := c.convert(done)
	if len(term) != 1 || !term[0].Terminal() || term[0].Kind != agent.EventCompleted {
		t.Fatalf("term %#v", term)
	}
	cancelled := &event.Event{
		Response:   &model.Response{Object: model.ObjectTypeRunnerCompletion, Done: true},
		RunOutcome: &event.RunOutcome{Status: event.RunOutcomeStatusCancelled},
		RequestID:  "run",
	}
	cancelEvt := c.convert(cancelled)
	if cancelEvt[0].Kind != agent.EventCancelled {
		t.Fatalf("cancel %#v", cancelEvt)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || (len(s) > 0 && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()))
}

func TestUsageDetailsPreserveUnknown(t *testing.T) {
	u := publicUsage(&model.Usage{TotalTokens: 111, PromptTokensDetails: model.PromptTokensDetails{CachedTokens: 12}, CompletionTokensDetails: model.CompletionTokensDetails{ReasoningTokens: 3}})
	if u.Source != "provider" || *u.CacheReadTokens != 12 || *u.ReasoningTokens != 3 || u.CacheWriteTokens != nil {
		t.Fatal(u)
	}
}
