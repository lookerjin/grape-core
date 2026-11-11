package trpcruntime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lookerjin/grape-core/config"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestNativeProviderRequests(t *testing.T) {
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
			if reply != "native reply" {
				t.Fatalf("reply %q", reply)
			}
		})
	}
}
