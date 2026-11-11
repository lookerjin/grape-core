package trpcruntime

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"

	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/modelaccess"
	infra "github.com/lookerjin/grape-core/model"
	openaiopt "github.com/openai/openai-go/option"
	"google.golang.org/genai"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/model/anthropic"
	"trpc.group/trpc-go/trpc-agent-go/model/gemini"
	"trpc.group/trpc-go/trpc-agent-go/model/openai"
)

// Agent factories need a model descriptor before the user has selected an account.
type unboundModel struct{ name string }

func (m unboundModel) Info() model.Info {
	n, _ := model.LookupModelContextWindow(m.name)
	return model.Info{Name: m.name, ContextWindow: n}
}

type turnModelKey struct{}

func (unboundModel) GenerateContent(ctx context.Context, req *model.Request) (<-chan *model.Response, error) {
	if m, ok := ctx.Value(turnModelKey{}).(model.Model); ok {
		return m.GenerateContent(ctx, req)
	}
	return nil, infra.ErrNotConfigured
}
func (rt *Runtime) selectModel(ctx context.Context, q agent.RunRequest) (model.Model, agent.ModelInfo, string, error) {
	caps := rt.modelInfo(rt.model.Info())
	if q.ModelRef == nil {
		if rt.registry != nil {
			return nil, caps, "", infra.ErrNotConfigured
		}
		if q.ModelName != "" && q.ModelName != rt.model.Info().Name && !rt.namedModels[q.ModelName] {
			return nil, caps, "", agent.Invalid("model_name", infra.ErrNotConfigured)
		}
		if rt.injectedModel || rt.fixed != nil {
			return nil, caps, "", nil
		}
		cfg := rt.cfg
		if q.ModelName != "" {
			cfg.Model.Name = q.ModelName
		}
		if cfg.Model.Name == "" {
			return nil, caps, "", infra.ErrNotConfigured
		}
		k := os.Getenv(cfg.Model.APIKeyEnv)
		m, e := providerWithKey(cfg, k)
		if e != nil {
			return nil, caps, "", e
		}
		return &countingModel{inner: m, shared: &rt.model.calls}, caps, k, nil
	}
	if rt.registry == nil || rt.fixed != nil || rt.injectedModel || q.ModelName != "" {
		return nil, caps, "", agent.Invalid("model_ref", infra.ErrNotConfigured)
	}
	resolve := rt.registry.Resolve
	if bg, _ := ctx.Value(backgroundModelKey{}).(bool); bg || rt.evolution != nil {
		resolve = rt.registry.ResolveBackground
	}
	b, e := resolve(ctx, infra.Scope{App: rt.appName, User: q.UserID}, *q.ModelRef)
	if e != nil {
		return nil, caps, "", e
	}
	id := b.Ref.ModelID
	k := b.NativeAPIKey(modelaccess.Native{})
	client := b.Client(modelaccess.Native{})
	client.Transport = observedTransport{client.Transport}
	endpoint := b.Provider.Endpoint
	var m model.Model
	var protocol string
	switch b.Provider.Protocol {
	case infra.Chat:
		protocol = modelProtocol("openai_compatible", endpoint)
		m = openai.New(id, openai.WithBaseURL(endpoint), openai.WithAPIKey(k), openai.WithOpenAIOptions(openaiopt.WithHTTPClient(client)))
	case infra.Messages:
		protocol = "anthropic"
		m = anthropic.New(id, anthropic.WithBaseURL(endpoint), anthropic.WithAPIKey(k), anthropic.WithAnthropicClientOptions(anthropicopt.WithHTTPClient(client)))
	case infra.Gemini:
		protocol = "gemini"
		m, e = gemini.New(ctx, id, gemini.WithGeminiClientConfig(&genai.ClientConfig{Backend: genai.BackendGeminiAPI, APIKey: k, HTTPClient: client, HTTPOptions: genai.HTTPOptions{BaseURL: endpoint}}))
	default:
		return nil, caps, "", errors.New("unsupported protocol")
	}
	if e != nil {
		return nil, caps, "", errors.New("model client initialization failed")
	}
	caps = capabilities(m.Info(), protocol, true)
	return &countingModel{inner: m, owner: b.Ref.ConnectionID, shared: &rt.model.calls, caps: &caps}, caps, k, nil
}
func splitFileID(raw string) (owner, id string) {
	if tail, ok := strings.CutPrefix(raw, "grape-file/"); ok {
		owner, id, _ = strings.Cut(tail, "/")
		return owner, id
	}
	return "", raw
}

// Unwrap only the selected account's references in a private copy of the projected request.
func projectFiles(req *model.Request, owner string, caps *agent.ModelInfo) (*model.Request, error) {
	if req == nil {
		return nil, nil
	}
	out := req
	for i, msg := range req.Messages {
		cloned := false
		for j, part := range msg.ContentParts {
			if caps != nil && (part.File != nil || part.Image != nil) && !caps.Multipart {
				return nil, errors.New("selected adapter cannot project session attachments")
			}
			if caps != nil && caps.Provider == "anthropic" && part.File != nil && part.File.FileID == "" && part.File.MimeType != "application/pdf" && part.File.MimeType != "text/plain" {
				return nil, errors.New("unsupported historical document MIME")
			}
			if part.File == nil || part.File.FileID == "" {
				continue
			}
			origin, id := splitFileID(part.File.FileID)
			if origin != owner || !fileIDPattern.MatchString(id) {
				return nil, infra.ErrAttachmentScope
			}
			if origin == "" {
				continue
			}
			if out == req {
				c := *req
				c.Messages = slices.Clone(req.Messages)
				out = &c
			}
			if !cloned {
				out.Messages[i].ContentParts = slices.Clone(msg.ContentParts)
				cloned = true
			}
			file := *part.File
			file.FileID = id
			out.Messages[i].ContentParts[j].File = &file
		}
	}
	return out, nil
}

type observedTransport struct{ http.RoundTripper }

func (t observedTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	res, e := t.RoundTripper.RoundTrip(q)
	if st := stateFrom(q.Context()); st != nil {
		if id, _ := invocationMeta(q.Context()); id == st.rootInvocation() {
			var class string
			if res != nil {
				class = res.Header.Get("X-Grape-Model-Error")
			}
			st.httpClass.Store(&class)
		}
	}
	return res, e
}
