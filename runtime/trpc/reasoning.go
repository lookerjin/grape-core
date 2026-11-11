package trpcruntime

import (
	"fmt"
	"slices"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// These are wire capabilities of the pinned adapters. Custom endpoints decide
// model eligibility themselves. Unknown never implies positive entitlement.
func reasoningSupport(provider, name string, known bool) (effort, budget string) {
	if !known {
		return "unknown", "unknown"
	}
	switch provider {
	case "", "openai", "openai_compatible", "deepseek", "qwen", "glm", "kimi", "minimax":
		return "unknown", "unsupported"
	case "anthropic":
		switch name {
		case "claude-opus-4-6", "claude-sonnet-4-6", "claude-opus-4-7", "claude-opus-4-8":
			return "supported", "unsupported"
		case "claude-sonnet-4-5", "claude-haiku-4-5", "claude-opus-4-5":
			return "unsupported", "supported"
		}
	case "gemini":
		if strings.HasPrefix(name, "gemini-3") {
			return "supported", "unsupported"
		}
		if strings.HasPrefix(name, "gemini-2.5-") {
			return "unsupported", "supported"
		}
	default:
		return "unsupported", "unsupported"
	}
	return "unknown", "unknown"
}

func reasoningPatch(c agent.ModelInfo, opt agent.ReasoningOptions, maxTokens *int) (*model.GenerationConfigPatch, error) {
	if opt == (agent.ReasoningOptions{}) {
		return nil, nil
	}
	invalid := func(reason string) (*model.GenerationConfigPatch, error) {
		return nil, agent.Invalid("reasoning", fmt.Errorf("%s (%s, %s)", reason, c.Provider, c.Name))
	}
	if !c.KnownAdapter {
		return invalid("unverified adapter")
	}
	if opt.Effort != "" && opt.BudgetTokens != 0 {
		return invalid("select effort or budget")
	}
	patch := &model.GenerationConfigPatch{}
	if opt.BudgetTokens != 0 {
		if c.ThinkingBudgetSupport != "supported" || opt.BudgetTokens < 1 || opt.BudgetTokens > 2147483647 {
			return invalid("unsupported thinking budget")
		}
		if c.Provider == "anthropic" {
			ceiling := 4096
			if maxTokens != nil && *maxTokens > 0 {
				ceiling = *maxTokens
			}
			if opt.BudgetTokens < 1024 || opt.BudgetTokens >= ceiling {
				return invalid("budget must be at least 1024 and below max output")
			}
			on := true
			patch.ThinkingEnabled = &on
		}
		patch.ThinkingTokens = &opt.BudgetTokens
		return patch, nil
	}
	levels := []string{"low", "medium", "high"}
	switch c.Provider {
	case "anthropic":
		if c.ReasoningEffortSupport != "supported" {
			return invalid("unverified adaptive thinking model")
		}
		on := true
		patch.ThinkingEnabled = &on
	case "gemini":
		if c.ReasoningEffortSupport != "supported" {
			return invalid("unverified thinking level model")
		}
		levels = []string{"low", "high"}
	case "deepseek":
		levels = []string{"high", "max"}
	default:
		if c.ReasoningEffortSupport == "unsupported" {
			return invalid("unsupported effort")
		}
	}
	if !slices.Contains(levels, opt.Effort) {
		return invalid("unsupported effort level")
	}
	if c.Provider == "gemini" {
		level := strings.ToUpper(opt.Effort)
		patch.ThinkingLevel = &level
	} else {
		patch.ReasoningEffort = &opt.Effort
	}
	return patch, nil
}
