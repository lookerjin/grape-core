package trpcruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lookerjin/grape-core/agent"
	infra "github.com/lookerjin/grape-core/model"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lookerjin/grape-core/config"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestNativeProviderRequests(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	reg, err := infra.Open(infra.Options{Scope: infra.Scope{App: "wire", User: "user"}, Database: filepath.Join(dir, "models.db"), Store: &infra.MemoryStore{}, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	conf := config.Default()
	stream := false
	conf.Model.Generation.Stream = &stream
	rt, err := New(Options{AppName: "wire", Config: &conf, DataDir: dir, ModelRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for _, tc := range []struct{ provider, path, header, input, response string }{
		{"anthropic", "/v1/messages", "X-Api-Key", "messages", `{"id":"one","model":"demo","role":"assistant","type":"message","content":[{"type":"text","text":"native reply"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`},
		{"gemini", "/v1beta/models/demo:generateContent", "X-Goog-Api-Key", "contents", `{"candidates":[{"content":{"role":"model","parts":[{"text":"native reply"}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.Method != "POST" || r.URL.Path != tc.path || r.Header.Get(tc.header) != "test-native-key" || body[tc.input] == nil {
					t.Errorf("unexpected native request: %s %s, credential present=%t, %s present=%t", r.Method, r.URL.Path, r.Header.Get(tc.header) != "", tc.input, body[tc.input] != nil)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			cfg := config.Default()
			cfg.Model.Provider = tc.provider
			cfg.Model.Name = "demo"
			cfg.Model.BaseURL = server.URL
			cfg.Model.APIKeyEnv = "GRAPE_TEST_KEY"
			t.Setenv(cfg.Model.APIKeyEnv, "test-native-key")
			mdl, err := newProviderModel(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := mdl.GenerateContent(context.Background(), &model.Request{Messages: []model.Message{model.NewUserMessage("hi")}})
			if err != nil {
				t.Fatal(err)
			}
			var reply string
			for r := range ch {
				if r.Error != nil {
					t.Fatalf("provider: %v", r.Error)
				}
				for _, choice := range r.Choices {
					reply += choice.Message.Content
				}
			}
			protocol := infra.Messages
			if tc.provider == "gemini" {
				protocol = infra.Gemini
			}
			c, err := reg.AddKey(ctx, infra.Provider{ID: tc.provider, Protocol: protocol, Endpoint: server.URL}, tc.provider, "test-native-key")
			if err != nil {
				t.Fatal(err)
			}
			h, err := rt.Start(ctx, agent.RunRequest{UserID: "user", SessionID: "shared", Message: "hello", ModelRef: &agent.ModelRef{ConnectionID: c.ID, ModelID: "demo"}})
			if err != nil {
				t.Fatal(err)
			}
			for range h.Events() {
			}
			if status := h.Status(); status.Status != "completed" || status.ConnectionID != c.ID {
				t.Fatal(status)
			}
			if reply != "native reply" {
				t.Fatalf("reply %q", reply)
			}
		})
	}
}

func TestPerRunReasoningOnNativeWire(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	registry, e := infra.Open(infra.Options{Scope: infra.Scope{App: "reason", User: "u"}, Database: filepath.Join(dir, "connections.db"), Store: &infra.MemoryStore{}, AllowLoopbackHTTP: true})
	if e != nil {
		t.Fatal(e)
	}
	defer registry.Close()
	cfg := config.Default()
	cfg.Model.Generation.Stream = boolPtr(false)
	rt, e := New(Options{Config: &cfg, AppName: "reason", DataDir: dir, ModelRegistry: registry})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	for _, tc := range []struct {
		protocol infra.Protocol
		model    string
		opt      agent.ReasoningOptions
		match    string
		response string
	}{
		{infra.Chat, "custom", agent.ReasoningOptions{Effort: "medium"}, `"reasoning_effort":"medium"`, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
		{infra.Messages, "claude-sonnet-4-6", agent.ReasoningOptions{Effort: "high"}, `"effort":"high"`, `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
		{infra.Messages, "claude-sonnet-4-5", agent.ReasoningOptions{BudgetTokens: 1024}, `"budget_tokens":1024`, `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
		{infra.Gemini, "gemini-2.5-flash", agent.ReasoningOptions{BudgetTokens: 256}, `"thinkingBudget":256`, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`},
		{infra.Gemini, "gemini-3-pro-preview", agent.ReasoningOptions{Effort: "low"}, `"thinkingLevel":"LOW"`, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`},
	} {
		t.Run(tc.model, func(t *testing.T) {
			var n atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if has := strings.Contains(string(raw), tc.match); has != (n.Add(1) == 1) {
					t.Errorf("unexpected reasoning field presence: %v", has)
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.response)
			}))
			defer s.Close()
			c, e := registry.AddKey(ctx, infra.Provider{ID: "fixture", Endpoint: s.URL, Protocol: tc.protocol}, "wire", "key")
			if e != nil {
				t.Fatal(e)
			}
			q := agent.RunRequest{UserID: "u", SessionID: "same", Message: "test", ModelRef: &agent.ModelRef{ConnectionID: c.ID, ModelID: tc.model}, Reasoning: &tc.opt}
			for i := 0; i < 2; i++ {
				h, e := rt.Start(ctx, q)
				if e != nil {
					t.Fatal(e)
				}
				collect(t, h)
				if h.Status().Status != "completed" {
					t.Fatal(h.Status())
				}
				q.Reasoning = nil
			}
			q.Reasoning = &agent.ReasoningOptions{Effort: "made-up"}
			if _, e := rt.Start(ctx, q); !errors.Is(e, agent.ErrInvalidConfig) {
				t.Fatal(e)
			}
			if n.Load() != 2 {
				t.Fatal(n.Load())
			}
		})
	}
}
