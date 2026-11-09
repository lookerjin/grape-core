// Package mockmodel is a network-free model.Model for tests and the offline
// example. It is not a product API.
package mockmodel

import (
	"context"
	"sync"
	"sync/atomic"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// Func builds the next model response from the request.
type Func func(ctx context.Context, req *model.Request) (*model.Response, error)

// Model implements model.Model.
type Model struct {
	name   string
	fn     Func
	stream bool
	block  bool

	calls atomic.Int64
	mu    sync.Mutex
	reqs  []*model.Request
}

// New returns a model that answers with fn.
func New(name string, fn Func) *Model {
	if name == "" {
		name = "grape-mock"
	}
	return &Model{name: name, fn: fn}
}

// WithStream splits a final text response into two deltas plus the full
// message so callers can observe a stream.
func (m *Model) WithStream() *Model {
	m.stream = true
	return m
}

// WithBlock waits until ctx is cancelled and then returns ctx.Err().
func (m *Model) WithBlock() *Model {
	m.block = true
	return m
}

// Calls is the number of GenerateContent invocations.
func (m *Model) Calls() int64 {
	return m.calls.Load()
}

// Requests returns a snapshot of observed requests.
func (m *Model) Requests() []*model.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.Request, len(m.reqs))
	copy(out, m.reqs)
	return out
}

// Info implements model.Model.
func (m *Model) Info() model.Info {
	return model.Info{Name: m.name}
}

// GenerateContent implements model.Model.
func (m *Model) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	m.calls.Add(1)
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	if m.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	rsp, err := m.fn(ctx, req)
	if err != nil {
		return nil, err
	}
	if rsp == nil {
		rsp = &model.Response{
			Done: true,
			Choices: []model.Choice{{
				Message: model.NewAssistantMessage(""),
			}},
		}
	}
	ch := make(chan *model.Response, 4)
	go func() {
		defer close(ch)
		if m.stream {
			text := ""
			if len(rsp.Choices) > 0 {
				text = rsp.Choices[0].Message.Content
			}
			if text != "" && len(rsp.Choices) > 0 && len(rsp.Choices[0].Message.ToolCalls) == 0 {
				mid := len(text) / 2
				if mid == 0 {
					mid = len(text)
				}
				ch <- &model.Response{
					Object:    model.ObjectTypeChatCompletionChunk,
					IsPartial: true,
					Choices: []model.Choice{{
						Delta: model.Message{Role: model.RoleAssistant, Content: text[:mid]},
					}},
				}
				if mid < len(text) {
					ch <- &model.Response{
						Object:    model.ObjectTypeChatCompletionChunk,
						IsPartial: true,
						Choices: []model.Choice{{
							Delta: model.Message{Role: model.RoleAssistant, Content: text[mid:]},
						}},
					}
				}
			}
		}
		if rsp.Object == "" && len(rsp.Choices) > 0 && len(rsp.Choices[0].Message.ToolCalls) == 0 {
			rsp.Object = model.ObjectTypeChatCompletion
		}
		rsp.Done = true
		ch <- rsp
	}()
	return ch, nil
}

// Echo replies with the last user message prefixed by "ack:".
func Echo(ctx context.Context, req *model.Request) (*model.Response, error) {
	msg := lastUser(req)
	return &model.Response{
		Choices: []model.Choice{{
			Message: model.NewAssistantMessage("ack: " + msg),
		}},
	}, nil
}

// ToolThenText emits one tool call on the first request and a final answer
// after a tool result is present.
func ToolThenText(toolName, args, final string) Func {
	return func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolResult(req) {
			return &model.Response{
				Choices: []model.Choice{{
					Message: model.NewAssistantMessage(final),
				}},
			}, nil
		}
		return &model.Response{
			Choices: []model.Choice{{
				Message: model.Message{
					Role: model.RoleAssistant,
					ToolCalls: []model.ToolCall{{
						ID:   "call-echo",
						Type: "function",
						Function: model.FunctionDefinitionParam{
							Name:      toolName,
							Arguments: []byte(args),
						},
					}},
				},
			}},
		}, nil
	}
}

func lastUser(req *model.Request) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == model.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

func hasToolResult(req *model.Request) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			return true
		}
	}
	return false
}
