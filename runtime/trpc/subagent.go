package trpcruntime

import (
	"reflect"
	"unsafe"

	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/llmagent"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

// wrapSubAgentTools gates the tool instances attached to a subagent.
// Matching a tool by name is not enough: a child can reuse run_command
// or another parent tool name and must still be checked on its own.
func (rt *Runtime) wrapSubAgentTools(ag trpcagent.Agent) {
	llm, ok := ag.(*llmagent.LLMAgent)
	if !ok || rt == nil {
		return
	}
	field := reflect.ValueOf(llm).Elem().FieldByName("tools")
	if !field.IsValid() || field.Kind() != reflect.Slice {
		return
	}
	slice := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	current, ok := slice.Interface().([]tool.Tool)
	if !ok {
		return
	}
	wrapped := make([]tool.Tool, len(current))
	for i, item := range current {
		wrapped[i] = rt.gate(item, "side_effect")
	}
	slice.Set(reflect.ValueOf(wrapped))
}
