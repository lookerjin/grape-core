package trpcruntime

import (
	"fmt"
	"slices"
	"strings"
	"trpc.group/trpc-go/trpc-agent-go/model"

	"github.com/lookerjin/grape-core/agent"
	infra "github.com/lookerjin/grape-core/model"
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

	effort, budget := reasoningSupport(p, i.Name, known)
	return agent.ModelInfo{ReasoningEffortSupport: effort, ThinkingBudgetSupport: budget, Name: i.Name, ContextWindow: i.ContextWindow, Provider: p, KnownAdapter: known, Multipart: multi, JSONSchema: multi && p != "anthropic"}
}

// ConnectionModelInfo describes the native wire adapter without contacting
// the provider or resolving credentials. It does not establish entitlement.
func ConnectionModelInfo(c infra.Connection, name string) agent.ModelInfo {
	protocol := ""
	switch c.Provider.Protocol {
	case infra.Chat:
		protocol = modelProtocol("openai_compatible", c.Provider.Endpoint)
	case infra.Messages:
		protocol = "anthropic"
	case infra.Gemini:
		protocol = "gemini"
	default:
		return capabilities(unboundModel{name}.Info(), "unknown", false)
	}
	return capabilities(unboundModel{name}.Info(), protocol, true)
}
