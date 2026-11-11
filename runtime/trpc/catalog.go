package trpcruntime

import (
	"fmt"
	"slices"
	"strings"
	"trpc.group/trpc-go/trpc-agent-go/model"

	"github.com/lookerjin/grape-core/agent"
)

func (rt *Runtime) prepareModels(injected bool) error {
	rt.namedModels = map[string]bool{}
	for _, name := range rt.cfg.Model.Alternatives {
		if name == "" || name != strings.TrimSpace(name) || injected || rt.fixed != nil {
			return agent.Invalid("model.alternatives", fmt.Errorf("requires named models from the configured provider"))
		}
		if name != rt.model.Info().Name {
			rt.namedModels[name] = true
		}
	}
	return nil
}

// Models returns configured choices, with adapter (not remote model) capabilities.
func (rt *Runtime) Models() []agent.ModelInfo {
	out := []agent.ModelInfo{rt.modelInfo(rt.model.Info())}
	for name := range rt.namedModels {
		out = append(out, rt.modelInfo(unboundModel{name: name}.Info()))
	}
	slices.SortFunc(out[1:], func(a, b agent.ModelInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (rt *Runtime) modelInfo(i model.Info) agent.ModelInfo {
	return capabilities(i, modelProtocol(rt.cfg.Model.Provider, rt.cfg.Model.BaseURL), !rt.injectedModel)
}
func capabilities(i model.Info, p string, known bool) agent.ModelInfo {
	multi := known && slices.Contains([]string{"", "openai", "openai_compatible", "gemini", "anthropic"}, p)

	return agent.ModelInfo{Name: i.Name, ContextWindow: i.ContextWindow, Provider: p, KnownAdapter: known, Multipart: multi, JSONSchema: multi && p != "anthropic"}
}
