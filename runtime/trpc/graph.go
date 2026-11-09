package trpcruntime

import (
	"context"
	"reflect"
	"strings"

	trpcagent "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/agent/graphagent"
	"trpc.group/trpc-go/trpc-agent-go/graph"
)

// UpperGraph is a small graph agent that uppercases the user input.
// It shows the graph entry point without adding a second scheduler.
func UpperGraph() (trpcagent.Agent, error) {
	schema := graph.NewStateSchema().
		AddField(graph.StateKeyUserInput, graph.StateField{
			Type:    reflect.TypeOf(""),
			Reducer: graph.DefaultReducer,
		}).
		AddField("out", graph.StateField{
			Type:    reflect.TypeOf(""),
			Reducer: graph.DefaultReducer,
		})
	compiled, err := graph.NewStateGraph(schema).
		AddNode("upper", func(ctx context.Context, state graph.State) (any, error) {
			in, _ := state[graph.StateKeyUserInput].(string)
			return graph.State{"out": strings.ToUpper(in)}, nil
		}).
		SetEntryPoint("upper").
		SetFinishPoint("upper").
		Compile()
	if err != nil {
		return nil, err
	}
	return graphagent.New("upper", compiled, graphagent.WithDescription("Uppercase the user message"))
}
