package trpcruntime

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/huggingface"
	"trpc.group/trpc-go/trpc-agent-go/model/hunyuan"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// countingModel counts GenerateContent calls without changing behavior.
type countingModel struct {
	inner model.Model
	calls atomic.Int64
}

func (m *countingModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	m.calls.Add(1)
	return m.inner.GenerateContent(ctx, req)
}

func (m *countingModel) Info() model.Info { return m.inner.Info() }

// buildModel returns the injected model or a tRPC provider model.
//
// gemini and anthropic are wired through the OpenAI-compatible client.
// tRPC v1.12.0 does not ship a separate native Gemini or Anthropic package.
// hunyuan uses secret id plus secret key environment variables.
func buildModel(cfg config.Config, override model.Model) (model.Model, *countingModel, error) {
	var inner model.Model
	if override != nil {
		inner = override
	} else {
		built, err := newProviderModel(cfg)
		if err != nil {
			return nil, nil, err
		}
		inner = built
	}
	wrapped := &countingModel{inner: inner}
	return wrapped, wrapped, nil
}

func newProviderModel(cfg config.Config) (model.Model, error) {
	if cfg.Model.APIKeyEnv == "" {
		return nil, agent.Invalid("model.api_key_env", fmt.Errorf("api_key_env is required when no model is injected"))
	}
	key := os.Getenv(cfg.Model.APIKeyEnv)
	if key == "" {
		return nil, agent.Invalid("model.api_key_env", fmt.Errorf("environment %s is empty", cfg.Model.APIKeyEnv))
	}
	name := cfg.Model.Name
	if name == "" {
		return nil, agent.Invalid("model.name", fmt.Errorf("model name is required"))
	}
	switch cfg.Model.Provider {
	case "", "openai", "openai_compatible", "gemini", "anthropic", "deepseek", "qwen", "glm", "kimi", "minimax":
		return newOpenAI(cfg, name, key), nil
	case "hunyuan":
		if cfg.Model.SecretIDEnv == "" {
			return nil, agent.Invalid("model.secret_id_env", fmt.Errorf("hunyuan requires secret_id_env"))
		}
		secretID := os.Getenv(cfg.Model.SecretIDEnv)
		if secretID == "" {
			return nil, agent.Invalid("model.secret_id_env", fmt.Errorf("environment %s is empty", cfg.Model.SecretIDEnv))
		}
		return hunyuan.New(name, hunyuan.WithSecretId(secretID), hunyuan.WithSecretKey(key)), nil
	case "huggingface":
		opts := []huggingface.Option{huggingface.WithAPIKey(key)}
		if cfg.Model.BaseURL != "" {
			opts = append(opts, huggingface.WithBaseURL(cfg.Model.BaseURL))
		}
		return huggingface.New(name, opts...)
	default:
		return nil, agent.Invalid("model.provider", fmt.Errorf("unsupported provider %q", cfg.Model.Provider))
	}
}

func newOpenAI(cfg config.Config, name, key string) model.Model {
	opts := []openai.Option{openai.WithAPIKey(key)}
	if cfg.Model.BaseURL != "" {
		opts = append(opts, openai.WithBaseURL(cfg.Model.BaseURL))
	}
	switch cfg.Model.Provider {
	case "deepseek":
		opts = append(opts, openai.WithVariant(openai.VariantDeepSeek))
	case "qwen":
		opts = append(opts, openai.WithVariant(openai.VariantQwen))
	case "glm":
		opts = append(opts, openai.WithVariant(openai.VariantGLM))
	case "kimi":
		opts = append(opts, openai.WithVariant(openai.VariantKimi))
	case "minimax":
		opts = append(opts, openai.WithVariant(openai.VariantMiniMax))
	}
	return openai.New(name, opts...)
}

func generationConfig(cfg config.Config) model.GenerationConfig {
	gen := model.GenerationConfig{}
	if cfg.Model.Generation.Stream != nil {
		gen.Stream = *cfg.Model.Generation.Stream
	}
	gen.Temperature = cfg.Model.Generation.Temperature
	gen.MaxTokens = cfg.Model.Generation.MaxTokens
	gen.TopP = cfg.Model.Generation.TopP
	return gen
}

func positiveLimit(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
