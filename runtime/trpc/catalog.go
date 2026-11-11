package trpcruntime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lookerjin/grape-core/agent"
)

func (rt *Runtime) prepareModels(injected bool) error {
	rt.namedModels = map[string]*countingModel{}
	for _, name := range rt.cfg.Model.Alternatives {
		if name == "" || name != strings.TrimSpace(name) || injected || rt.fixed != nil {
			return agent.Invalid("model.alternatives", fmt.Errorf("requires named models from the configured provider"))
		}
		if name == rt.model.Info().Name || rt.namedModels[name] != nil {
			continue
		}
		cfg := rt.cfg
		cfg.Model.Name = name
		_, m, err := buildModel(cfg, nil)
		if err != nil {
			return err
		}
		rt.namedModels[name] = m
	}
	return nil
}

// Models returns configured choices, with adapter (not remote model) capabilities.
func (rt *Runtime) Models() []agent.ModelInfo {
	out := []agent.ModelInfo{rt.modelInfo(rt.counter)}
	for _, m := range rt.namedModels {
		out = append(out, rt.modelInfo(m))
	}
	slices.SortFunc(out[1:], func(a, b agent.ModelInfo) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (rt *Runtime) modelInfo(m *countingModel) agent.ModelInfo {
	p := rt.protocol()
	multi := !rt.injectedModel && slices.Contains([]string{"", "openai", "openai_compatible", "gemini", "anthropic"}, p)
	i := m.Info()
	return agent.ModelInfo{Name: i.Name, ContextWindow: i.ContextWindow, Provider: p, KnownAdapter: !rt.injectedModel, Multipart: multi, JSONSchema: multi && p != "anthropic"}
}
