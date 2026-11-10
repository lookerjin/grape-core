package trpcruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool/agent"
	skilltool "trpc.group/trpc-go/trpc-agent-go/tool/skill"
)

// toolPermission is the only tool authorization decision. tRPC calls it
// after BeforeTool callbacks and before the tool runs. Shell hooks and
// stdio process launch are not decided here.
func (rt *Runtime) toolPermission(ctx context.Context, req *tool.PermissionRequest) (tool.PermissionDecision, error) {
	if rt == nil || req == nil {
		return tool.DenyPermission("missing permission request"), nil
	}
	item := frameworkNamed(req.Tool)
	if t, ok := adapted(item); ok && t.trusted {
		return tool.AllowPermission(), nil
	}
	name := req.ToolName
	if name == "" && item != nil && item.Declaration() != nil {
		name = item.Declaration().Name
	}
	if _, ok := item.(tool.CallableTool); !ok {
		rt.recordDecision(ctx, "side_effect", name, "", false, "tool is not callable")
		return tool.DenyPermission("tool is not callable"), nil
	}
	kind := rt.toolKind(ctx, item, name)
	detail, path := approvalTarget(name, req.Arguments)
	if err := rt.authorize(ctx, kind, detail, path); err != nil {
		return tool.DenyPermission(err.Error()), nil
	}
	return tool.AllowPermission(), nil
}

// approvalTarget recovers the argv or file path the decider used to see.
// The tool name remains the detail when the arguments do not carry either.
func approvalTarget(name string, args []byte) (detail, path string) {
	detail = name
	if len(args) == 0 {
		return detail, ""
	}
	var body struct {
		Argv     []string `json:"argv"`
		FileName string   `json:"file_name"`
		Skill    string   `json:"skill"`
		Command  string   `json:"command"`
	}
	if err := json.Unmarshal(args, &body); err != nil {
		return detail, ""
	}
	if len(body.Argv) > 0 {
		return strings.Join(body.Argv, " "), ""
	}
	if body.Command != "" {
		if body.Skill != "" {
			return body.Skill + ": " + body.Command, ""
		}
		return body.Command, ""
	}
	if body.FileName != "" {
		return body.FileName, body.FileName
	}
	return detail, ""
}

// isGrapeRead reports grape's own read types. A same-sized foreign tool
// does not qualify, and neither does a caller Original wrapper.
func isGrapeRead(item tool.Tool) bool {
	if t, ok := adapted(item); ok {
		return t.kind == "read"
	}
	switch t := item.(type) {
	case *hostMCPTool:
		return t != nil && t.kind == "read"
	case *skilltool.LoadTool, *skilltool.ListDocsTool, *skilltool.SelectDocsTool:
		return true
	default:
		return false
	}
}

func (rt *Runtime) toolKind(ctx context.Context, item tool.Tool, name string) string {
	inv, _ := trpcagent.InvocationFromContext(ctx)
	agentName := ""
	if inv != nil {
		agentName = inv.AgentName
	}
	if rt.allowNestedRead(agentName, name) {
		return "read"
	}
	if t, ok := adapted(item); ok {
		return t.kind
	}
	if isGrapeRead(item) {
		return "read"
	}
	switch item.(type) {
	case *hostMCPTool:
		return "write"
	case *agenttool.Tool:
		return "side_effect"
	default:
		return "side_effect"
	}
}

// v1.12.0 passes its internal NamedTool for ToolSet entries. It exposes no
// public semantic unwrap. Verify the wrapper type before trusting Original.
func frameworkNamed(item tool.Tool) tool.Tool {
	for i := 0; item != nil && i < 4; i++ {
		value := reflect.ValueOf(item)
		if value.Kind() != reflect.Ptr || value.IsNil() || value.Elem().Kind() != reflect.Struct {
			return item
		}
		typ := value.Elem().Type()
		if typ.PkgPath() != "trpc.group/trpc-go/trpc-agent-go/internal/tool" || typ.Name() != "NamedTool" {
			return item
		}
		named, ok := item.(interface{ Original() tool.Tool })
		if !ok {
			return item
		}
		inner := named.Original()
		if inner == nil {
			return item
		}
		item = inner
	}
	return item
}

func rejectRawAgentTool(item tool.Tool) error {
	if item == nil {
		return nil
	}
	if _, ok := item.(tool.CallableTool); !ok {
		return agent.Invalid("tool", fmt.Errorf("untrusted tools must implement Call"))
	}
	if _, ok := item.(*agenttool.Tool); ok {
		if err := rejectReservedTool(item); err != nil {
			return err
		}
		return agent.Invalid("tool", fmt.Errorf("a raw agent tool requires ToolSpec.Trusted; register child agents through Options.Subagents"))
	}
	return nil
}
