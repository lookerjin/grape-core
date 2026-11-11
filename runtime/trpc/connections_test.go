package trpcruntime_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	infra "github.com/lookerjin/grape-core/model"
	grape "github.com/lookerjin/grape-core/runtime/trpc"
)

func TestConnectionsAndScopedAttachments(t *testing.T) {
	ctx := context.Background()
	var aCalls, bCalls atomic.Int32
	server := func(key string, calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key {
				t.Error("wrong account")
			}
			b, _ := io.ReadAll(r.Body)
			if strings.Contains(string(b), "reject-this-request") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"private-error-marker","type":"auth_error"}}`)
				return
			}
			if strings.Contains(string(b), "grape-file/") {
				t.Error("internal reference sent to provider")
			}
			if key == "aaa" && strings.Contains(string(b), "with-file") && !strings.Contains(string(b), "file-123") {
				t.Error("provider file lost")
			}
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"ok","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}))
	}
	a := server("aaa", &aCalls)
	defer a.Close()
	b := server("bbb", &bCalls)
	defer b.Close()
	dir := t.TempDir()
	scope := infra.Scope{App: "p", User: "u"}
	opts := infra.Options{Scope: scope, Database: filepath.Join(dir, "models.db"), AllowLoopbackHTTP: true, Store: &infra.MemoryStore{}}
	reg, e := infra.Open(opts)
	if e != nil {
		t.Fatal(e)
	}
	defer reg.Close()
	add := func(s *httptest.Server, key string) agent.ModelRef {
		c, e := reg.AddKey(ctx, infra.Provider{ID: "custom", Endpoint: s.URL, Protocol: infra.Chat}, key+"-label", key)
		if e != nil {
			t.Fatal(e)
		}
		return agent.ModelRef{ConnectionID: c.ID, ModelID: "same"}
	}
	ar, br := add(a, "aaa"), add(b, "bbb")
	cfg := config.Default()
	stream := false
	cfg.Model.Generation.Stream = &stream
	cfg.Journal.Enabled = false
	open := func() *grape.Runtime {
		rt, e := grape.New(grape.Options{AppName: "p", Config: &cfg, ModelRegistry: reg, DataDir: dir})
		if e != nil {
			t.Fatal(e)
		}
		return rt
	}
	rt := open()
	defer func() { rt.Close() }()
	q := agent.RunRequest{UserID: "u", SessionID: "s", Message: "hello"}
	if _, e = rt.Start(ctx, q); !errors.Is(e, infra.ErrNotConfigured) {
		t.Fatal(e)
	}
	turn := func(ref agent.ModelRef, success bool) {
		t.Helper()
		if e := reg.SetDefault(ctx, ref); e != nil {
			t.Fatal(e)
		}
		h, e := rt.Start(ctx, q)
		if e != nil {
			if !success && errors.Is(e, infra.ErrAttachmentScope) {
				return
			}
			t.Fatal(e)
		}
		for range h.Events() {
		}
		if (h.Status().Status == "completed") != success {
			t.Fatalf("result %+v", h.Status())
		}
		if !success && h.Status().ModelError != "attachment_scope" {
			t.Fatalf("unclassified scope mismatch: %+v", h.Status())
		}
		if success {
			s, ok := h.(agent.RunInspector).ContextStatus()
			if !ok || s.ConnectionID != ref.ConnectionID || s.Model != "same" {
				t.Fatal(s)
			}
		}
	}
	turn(ar, true)
	turn(br, true)
	q.Message = "with-file"
	q.Attachments = []agent.Attachment{{Kind: "file", MIME: "text/plain", FileID: "file-123", ConnectionID: ar.ConnectionID}}
	turn(ar, true)
	q.Attachments = nil
	q.Message = "next"
	rt.Close()
	rt = open()
	history, e := rt.SessionHistory(ctx, "u", "s", 0)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, m := range history {
		for _, a := range m.Attachments {
			found = found || a.FileID == "file-123" && a.ConnectionID == ar.ConnectionID
		}
	}
	if !found {
		t.Fatal("attachment ownership lost after restart")
	}
	turn(br, false)
	turn(ar, true)
	if aCalls.Load() != 3 || bCalls.Load() != 1 {
		t.Fatalf("wire calls %d %d", aCalls.Load(), bCalls.Load())
	}
	q.SessionID = "failure"
	q.Message = "reject-this-request"
	q.ModelRef = &ar
	failed, e := rt.Start(ctx, q)
	if e != nil {
		t.Fatal(e)
	}
	for event := range failed.Events() {
		if strings.Contains(string(event.Payload), "private-error-marker") {
			t.Fatal("provider error body leaked")
		}
	}
	if status := failed.Status(); status.Status != "failed" || status.ModelError != "authentication" {
		t.Fatalf("wrong provider status: %+v", status)
	}
	q.UserID = "stranger"
	q.ModelRef = &ar
	if _, e := rt.Start(ctx, q); !errors.Is(e, infra.ErrNotConfigured) {
		t.Fatal(e)
	}
}
func TestModelCredentialAtStart(t *testing.T) {
	cfg := config.Default()
	cfg.Model.APIKeyEnv = "GRAPE_MISSING_TEST"
	t.Setenv(cfg.Model.APIKeyEnv, "")
	cfg.Model.Name = "valid"
	rt, e := grape.New(grape.Options{Config: &cfg, DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer rt.Close()
	_, e = rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "s", Message: "hello"})
	if !errors.Is(e, infra.ErrCredentialUnavailable) {
		t.Fatal(e)
	}
}
