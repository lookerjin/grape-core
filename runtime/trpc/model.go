package trpcruntime

import (
	"context"
	"errors"
	"fmt"
	infra "github.com/lookerjin/grape-core/model"
	"os"
	"sync/atomic"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"google.golang.org/genai"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/anthropic"
	"trpc.group/trpc-go/trpc-agent-go/model/gemini"
	"trpc.group/trpc-go/trpc-agent-go/model/huggingface"
	"trpc.group/trpc-go/trpc-agent-go/model/hunyuan"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// countingModel counts GenerateContent calls without changing behavior.
type countingModel struct {
	reasoning *model.GenerationConfigPatch
	inner     model.Model
	calls     atomic.Int64
	owner     string
	caps      *agent.ModelInfo
	shared    *atomic.Int64
}

func (m *countingModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	if m.shared != nil {
		m.shared.Add(1)
	} else {
		m.calls.Add(1)
	}
	checked, err := projectFiles(req, m.owner, m.caps)
	if err != nil {
		if st := stateFrom(ctx); st != nil {
			st.attachmentMismatch.Store(errors.Is(err, infra.ErrAttachmentScope))
		}
		return nil, err
	}
	req = checked
	if m.reasoning != nil {
		copyReq := *req
		copyReq.GenerationConfig = model.ApplyGenerationConfigPatch(req.GenerationConfig, *m.reasoning)
		req = &copyReq
	}
	stateFrom(ctx).noteProjection(ctx, m.Info(), req)
	return m.inner.GenerateContent(ctx, req)
}

func (m *countingModel) Info() model.Info { return m.inner.Info() }

func initialModel(cfg config.Config, injected model.Model) *countingModel {
	if injected == nil {
		injected = unboundModel{name: cfg.Model.Name}
	}
	return &countingModel{inner: injected}
}

func newProviderModel(cfg config.Config) (model.Model, error) {
	return providerWithKey(cfg, os.Getenv(cfg.Model.APIKeyEnv))
}
func providerWithKey(cfg config.Config, key string) (model.Model, error) {
	if key == "" {
		return nil, infra.ErrCredentialUnavailable
	}
	name := cfg.Model.Name
	if name == "" {
		return nil, infra.ErrNotConfigured
	}
	switch cfg.Model.Provider {
	case "gemini":
		return gemini.New(context.Background(), name, gemini.WithGeminiClientConfig(&genai.ClientConfig{
			Backend: genai.BackendGeminiAPI, APIKey: key, HTTPOptions: genai.HTTPOptions{BaseURL: cfg.Model.BaseURL},
		}))
	case "anthropic":
		opts := []anthropic.Option{anthropic.WithAPIKey(key)}
		if cfg.Model.BaseURL != "" {
			opts = append(opts, anthropic.WithBaseURL(cfg.Model.BaseURL))
		}
		return anthropic.New(name, opts...), nil
	case "", "openai", "openai_compatible", "deepseek", "qwen", "glm", "kimi", "minimax":
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

func positiveLimit(n *int) int {
	if n == nil || *n <= 0 {
		return 0
	}
	return *n
}
