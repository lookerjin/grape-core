package mockmodel

import (
	"context"
	"encoding/json"
	"fmt"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// CodingFix drives a small edit loop: search, read, replace, test, report.
// It is deterministic and does not call a network.
func CodingFix(ctx context.Context, req *model.Request) (*model.Response, error) {
	tools := toolResultCount(req)
	switch tools {
	case 0:
		return ToolCall("search_content", `{"path":".","file_pattern":"*.go","content_pattern":"func Add"}`), nil
	case 1:
		return ToolCall("read_file", `{"file_name":"math.go"}`), nil
	case 2:
		return ToolCall("replace_content", `{"file_name":"math.go","old_string":"return a - b","new_string":"return a + b","num_replacements":1}`), nil
	case 3:
		return ToolCall("run_command", `{"argv":["go","test","./..."],"timeout_sec":120}`), nil
	default:
		var command string
		for _, msg := range req.Messages {
			if msg.Role == model.RoleTool && msg.ToolName == "run_command" {
				command = msg.Content
			}
		}
		var result struct {
			ExitCode *int   `json:"exit_code"`
			Stdout   string `json:"stdout"`
		}
		msg := "fixture test did not pass: " + command
		if json.Unmarshal([]byte(command), &result) == nil && result.ExitCode != nil && *result.ExitCode == 0 {
			msg = "fixture test passed: " + result.Stdout
		}
		if _, ok := req.Tools["reviewer"]; ok && tools == 4 {
			args, err := json.Marshal(map[string]string{"request": fmt.Sprintf("Review this fixture test result: %s", msg)})
			if err != nil {
				return nil, err
			}
			return ToolCall("reviewer", string(args)), nil
		}
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(msg)}}}, nil
	}
}

func ToolCall(name, args string) *model.Response {
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
