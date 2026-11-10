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
	item := unwrapTracking(frameworkNamed(req.Tool))
	if _, ok := item.(interface{ grapeTrusted() }); ok {
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
	switch unwrapGrape(frameworkNamed(item)).(type) {
	case *builtinReadTool, *grapeReadTool:
		return true
	case *hostMCPTool:
		mcpTool := unwrapGrape(frameworkNamed(item)).(*hostMCPTool)
		return mcpTool != nil && mcpTool.kind == "read"
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
	if kind, ok := labeledKind(item); ok {
		if kind == "" {
			return "side_effect"
		}
		return kind
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

// unwrapTracking removes only the in-flight counter wrapper.
// Kind tags stay visible so the policy can read them.
func unwrapTracking(item tool.Tool) tool.Tool {
	for i := 0; item != nil && i < 8; i++ {
		switch current := item.(type) {
		case streamTracking:
			if current.inner == nil {
				return item
			}
			item = current.inner
		case trackingTool:
			if current.inner == nil {
				return item
			}
			item = current.inner
		default:
			return item
		}
	}
	return item
}

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
		method := value.MethodByName("Original")
		if !method.IsValid() || method.Type().NumIn() != 0 || method.Type().NumOut() != 1 {
			return item
		}
		out := method.Call(nil)
		inner, _ := out[0].Interface().(tool.Tool)
		if inner == nil {
			return item
		}
		item = inner
	}
	return item
}

func rejectUntrustedAgentTools(tools []tool.Tool, specs []ToolSpec) error {
	for _, item := range tools {
		if err := rejectRawAgentTool(item); err != nil {
			return err
		}
	}
	for _, spec := range specs {
		if spec.Trusted {
			continue
		}
		if err := rejectRawAgentTool(spec.Tool); err != nil {
			return err
		}
	}
	return nil
}

func rejectRawAgentTool(item tool.Tool) error {
	if item == nil {
		return nil
	}
	if _, ok := item.(*agenttool.Tool); ok {
		return agent.Invalid("tool", fmt.Errorf("a raw agent tool requires ToolSpec.Trusted; register child agents through Options.Subagents"))
	}
	return nil
}
