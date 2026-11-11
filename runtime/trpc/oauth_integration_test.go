package trpcruntime_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	infra "github.com/lookerjin/grape-core/model"
	grape "github.com/lookerjin/grape-core/runtime/trpc"
	"golang.org/x/oauth2"
)

func TestOAuthAgentLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var challenge atomic.Value
	var refresh, requests atomic.Int32
	var uri string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device":
			fmt.Fprintf(w, `{"device_code":"device","user_code":"DISPLAY","verification_uri":%q,"expires_in":30,"interval":1}`, uri+"/approve")
		case "/token":
			switch r.FormValue("grant_type") {
			case "refresh_token":
				if r.FormValue("refresh_token") != "initial" {
					t.Error("refresh token mismatch")
				}
				refresh.Add(1)
				fmt.Fprint(w, `{"access_token":"renewed","refresh_token":"rotated","token_type":"bearer","expires_in":3600}`)
			case "authorization_code":
				sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
				if r.FormValue("code") != "approved" || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge.Load() {
					t.Error("invalid PKCE")
				}
				fmt.Fprint(w, `{"access_token":"first","refresh_token":"initial","token_type":"Bearer","scope":"inference-api","expires_in":1}`)
			case "urn:ietf:params:oauth:grant-type:device_code":
				fmt.Fprint(w, `{"access_token":"renewed","refresh_token":"rotated","token_type":"Bearer","scope":"inference-api","expires_in":3600}`)
			default:
				http.Error(w, "invalid grant", 400)
			}
		case "/me":
			fmt.Fprint(w, `{"sub":"provider-account"}`)
		case "/v1/account/models":
			if r.Header.Get("Authorization") != "Bearer renewed" {
				t.Error("wrong discovery credential")
			}
			fmt.Fprint(w, `{"models":["research"]}`)
		case "/v1/tenant/infer":
			if r.Header.Get("Authorization") != "Bearer renewed" {
				t.Error("wrong inference credential")
			}
			requests.Add(1)
			var req struct {
				Effort   string `json:"reasoning_effort"`
				Stream   bool
				Messages []struct{ Role string }
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !req.Stream || req.Effort != "high" {
				t.Error("invalid streaming request")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			delta, stop := `{"role":"assistant","content":"finished"}`, "stop"
			if req.Messages[len(req.Messages)-1].Role == "user" {
				delta = `{"role":"assistant","tool_calls":[{"index":0,"id":"tool-list","type":"function","function":{"name":"list_file","arguments":"{}"}}]}`
				stop = "tool_calls"
			}
			fmt.Fprintf(w, "data: {\"id\":\"chunk\",\"object\":\"chat.completion.chunk\",\"model\":\"research\",\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":null}]}\n\ndata: {\"id\":\"chunk\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", delta, stop)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	uri = s.URL
	p := infra.NewOAuthProvider(infra.Provider{ID: "fixture", Protocol: infra.Chat, Endpoint: s.URL + "/v1"}, oauth2.Config{ClientID: "our-app", Scopes: []string{"inference-api"}, Endpoint: oauth2.Endpoint{AuthURL: s.URL + "/authorize", TokenURL: s.URL + "/token", DeviceAuthURL: s.URL + "/device"}}, s.URL+"/me", "inference-api")
	p = p.WithInferenceRoutes(infra.InferenceRoutes{Generate: "/tenant/infer", Models: "/account/models", DecodeModels: func(raw json.RawMessage) ([]string, error) {
		var v struct{ Models []string }
		e := json.Unmarshal(raw, &v)
		return v.Models, e
	}})
	o := infra.Options{Database: filepath.Join(t.TempDir(), "registry"), Scope: infra.Scope{App: "local", User: "u"}, AllowLoopbackHTTP: true, Store: &infra.MemoryStore{}, OAuth: []infra.OAuthProvider{p}}
	reg, e := infra.Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer reg.Close()
	login, e := reg.BeginLogin(ctx, "fixture", "http://127.0.0.1:0/callback")
	if e != nil {
		t.Fatal(e)
	}
	defer login.Cancel()
	auth, _ := url.Parse(login.Challenge.URL)
	params := auth.Query()
	challenge.Store(params.Get("code_challenge"))
	callback := func(state string) int {
		res, e := http.Get(params.Get("redirect_uri") + "?code=approved&state=" + url.QueryEscape(state))
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res.StatusCode
	}
	if callback("wrong") != 400 || callback(params.Get("state")) != 200 {
		t.Fatal("callback state")
	}
	c, e := login.Complete("test")
	if e != nil {
		t.Fatal(e)
	}
	ref := infra.Ref{ConnectionID: c.ID, ModelID: "research"}
	other, e := infra.Open(o)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	var wg sync.WaitGroup
	for _, r := range []*infra.Registry{reg, other, reg, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := r.Resolve(ctx, o.Scope, ref); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if refresh.Load() != 1 {
		t.Fatalf("refreshes %d", refresh.Load())
	}
	if _, e = other.ResolveBackground(ctx, o.Scope, ref); !errors.Is(e, agent.ErrPermissionDenied) {
		t.Fatal(e)
	}
	if e = other.AllowBackground(ctx, c.ID, true); e != nil {
		t.Fatal(e)
	}
	device, e := other.BeginDevice(ctx, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if device.Challenge.UserCode != "DISPLAY" {
		t.Fatal(device.Challenge)
	}
	again, e := device.Complete("again")
	if e != nil || again.ID != c.ID {
		t.Fatal(again, e)
	}
	models, e := other.Discover(ctx, c.ID)
	if e != nil || len(models) != 1 || models[0].Availability != "unknown" {
		t.Fatal(models, e)
	}
	cfg := config.Default()
	cfg.Workspace.Root = t.TempDir()
	stream := true
	cfg.Model.Generation.Stream = &stream
	rt, e := grape.New(grape.Options{AppName: "local", ModelRegistry: other, Config: &cfg, DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	h, e := rt.Start(ctx, agent.RunRequest{UserID: "u", SessionID: "s", Message: "list files", Reasoning: &agent.ReasoningOptions{Effort: "high"}, ModelRef: &ref})
	if e != nil {
		t.Fatal(e)
	}
	text, tool := false, false
	for ev := range h.Events() {
		text = text || ev.Kind == agent.EventText && strings.Contains(string(ev.Payload), "finished")
		tool = tool || ev.Kind == agent.EventToolEnd
	}
	if h.Status().Status != "completed" || !text || !tool || requests.Load() != 2 {
		t.Fatalf("status %+v text=%v tool=%v calls=%d", h.Status(), text, tool, requests.Load())
	}
	models, e = other.Models(ctx, c.ID)
	if e != nil || len(models) != 1 || models[0].Availability != "available" {
		t.Fatal(models, e)
	}
	if e = other.Remove(ctx, c.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = reg.Resolve(ctx, o.Scope, ref); !errors.Is(e, infra.ErrNotConfigured) {
		t.Fatal(e)
	}
}
