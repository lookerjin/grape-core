package trpcruntime

import (
	"context"
	"encoding/json"
	"github.com/lookerjin/grape-core/agent"
	infra "github.com/lookerjin/grape-core/model"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
