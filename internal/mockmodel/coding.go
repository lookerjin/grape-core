package mockmodel

import (
	"context"
	"strings"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// CodingFix drives a small edit loop: search, read, replace, test, report.
// It is deterministic and does not call a network.
func CodingFix(ctx context.Context, req *model.Request) (*model.Response, error) {
	tools := toolResultCount(req)
	switch tools {
	case 0:
		return toolCall("search_content", `{"path":".","file_pattern":"*.go","content_pattern":"func Add"}`), nil
	case 1:
		return toolCall("read_file", `{"file_name":"math.go"}`), nil
	case 2:
		return toolCall("replace_content", `{"file_name":"math.go","old_string":"return a - b","new_string":"return a + b","num_replacements":1}`), nil
	case 3:
		return toolCall("run_command", `{"argv":["go","test","./..."],"timeout_sec":120}`), nil
	default:
		text := allText(req)
		msg := "updated math.go and reran go test"
		if strings.Contains(text, "ok") || strings.Contains(text, "PASS") {
			msg = "fixed Add and go test passed"
		}
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(msg)}}}, nil
	}
}

func toolCall(name, args string) *model.Response {
	return &model.Response{
		Choices: []model.Choice{{
			Message: model.Message{
				Role: model.RoleAssistant,
				ToolCalls: []model.ToolCall{{
					ID:   "call-" + name,
					Type: "function",
					Function: model.FunctionDefinitionParam{
						Name:      name,
						Arguments: []byte(args),
					},
				}},
			},
		}},
	}
}

func toolResultCount(req *model.Request) int {
	if req == nil {
		return 0
	}
	n := 0
	for _, msg := range req.Messages {
		if msg.Role == model.RoleTool {
			n++
		}
	}
	return n
}

func allText(req *model.Request) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range req.Messages {
		b.WriteString(msg.Content)
		b.WriteByte('\n')
	}
	return b.String()
}
