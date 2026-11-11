package trpcruntime_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/plugin"
	grape "github.com/lookerjin/grape-core/runtime/trpc"
	native "trpc.group/trpc-go/trpc-agent-go/agent"
	"trpc.group/trpc-go/trpc-agent-go/artifact"
	"trpc.group/trpc-go/trpc-agent-go/artifact/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

// Exercise the embedding API across the actual OpenAI wire adapter, runner and SQLite service.
func TestProductTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aoykAAAAASUVORK5CYII=")
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		if bytes.Contains(raw, []byte("UNSUPPORTED_MEDIA")) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"model does not accept files","type":"invalid_request_error"}}`)
			return
		}
		if bytes.Contains(raw, []byte("WAIT_HERE")) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-ctx.Done():
			}
			return
		}
		var body struct {
			Tools    []struct{ Function struct{ Name string } }
			Messages []struct {
				Role    string
				Content json.RawMessage
			}
			Format json.RawMessage `json:"response_format"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			return
		}
		result := map[string]any{"role": "assistant", "content": `{"result":"saved"}`}
		stop := "stop"
		if len(body.Tools) != 1 || body.Tools[0].Function.Name != "archive" {
			t.Errorf("unexpected agent tools: %+v", body.Tools)
		}
		if !bytes.Contains(raw, []byte("PLUGIN_INSTRUCTION")) {
			t.Error("plugin instruction missing")
		}
		if bytes.Contains(raw, []byte("INVALID_OUTPUT")) {
			result["content"] = `{"result":false}`
		} else if body.Messages[len(body.Messages)-1].Role != "tool" {
			if bytes.Contains(raw, []byte("compare sample")) {
				for _, part := range []string{"image_url", "file_data", "file-ABC", "evidence.txt", "json_schema"} {
					if !bytes.Contains(raw, []byte(part)) {
						t.Errorf("wire request lacks %s", part)
					}
				}
			}
			delete(result, "content")
			result["tool_calls"] = []any{map[string]any{"id": "call_archive", "type": "function", "function": map[string]string{"name": "archive", "arguments": "{}"}}}
			stop = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "object": "chat.completion", "model": "local", "choices": []any{map[string]any{"index": 0, "message": result, "finish_reason": stop}}})
	}))
	defer server.Close()
	root := t.TempDir()
	mgr, e := plugin.NewManager(filepath.Join(root, "installed"))
	if e != nil {
		t.Fatal(e)
	}
	src := filepath.Join(root, "bundle")
	for name, body := range map[string]string{".claude-plugin/plugin.json": `{"name":"fixture"}`, "agents/analyst.md": "---\ndescription: Local analyst\ntools: [archive]\n---\nPLUGIN_INSTRUCTION", "commands/compare.md": "---\narguments: [topic]\n---\ncompare {{topic}}"} {
		path := filepath.Join(src, name)
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, []byte(body), 0644); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := mgr.InstallLocal(src); e != nil {
		t.Fatal(e)
	}
	if e := mgr.Enable("fixture"); e != nil {
		t.Fatal(e)
	}
	cfg := config.Default()
	cfg.Model.APIKeyEnv = "GRAPE_PRODUCT_KEY"
	t.Setenv(cfg.Model.APIKeyEnv, "fixture-key")
	cfg.Model.BaseURL = server.URL
	cfg.Model.Name = "local"
	stream := false
	cfg.Model.Generation.Stream = &stream
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Approvals.Required = false
	backend := inmemory.NewService()
	archive := function.NewFunctionTool(func(ctx context.Context, _ struct{}) (int, error) {
		inv, ok := native.InvocationFromContext(ctx)
		if !ok || inv.ArtifactService == nil {
			return 0, fmt.Errorf("missing native service")
		}
		scope := artifact.SessionInfo{AppName: inv.Session.AppName, UserID: inv.Session.UserID, SessionID: inv.Session.ID}
		return inv.ArtifactService.SaveArtifact(ctx, scope, "report.txt", &artifact.Artifact{MimeType: "text/plain", Data: []byte("tool evidence")})
	}, function.WithName("archive"))
	opts := grape.Options{Config: &cfg, DataDir: filepath.Join(root, "data"), PluginManager: mgr, ArtifactService: backend, PluginTools: map[string][]grape.ToolSpec{"fixture/analyst": {{Tool: archive, Kind: "write"}}}}
	missing := opts
	missing.PluginTools = nil
	if invalid, err := grape.New(missing); err == nil {
		invalid.Close()
		t.Fatal("missing plugin grant accepted")
	}
	open := func() *grape.Runtime {
		rt, e := grape.New(opts)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { rt.Close() })
		return rt
	}
	rt := open()
	if len(rt.Definitions()) != 2 {
		t.Fatal(rt.Definitions())
	}
	req := agent.RunRequest{UserID: "local", SessionID: "main", AgentID: "analyst", Attachments: []agent.Attachment{{Kind: "image", MIME: "image/png", Data: png}, {Kind: "file", Name: "evidence.txt", MIME: "text/plain", Data: []byte("source")}, {Kind: "file", MIME: "application/pdf", FileID: "file-ABC"}}, Output: &agent.OutputSchema{Name: "report", Strict: true, Schema: json.RawMessage(`{"type":"object","required":["result"],"properties":{"result":{"type":"string"}},"additionalProperties":false}`)}}
	bad := req
	bad.Output = &agent.OutputSchema{Name: "bad", Schema: json.RawMessage(`{"type":"object","$ref":"file:///forbidden"}`)}
	bad.Message = "check"
	if _, e := rt.Start(ctx, bad); e == nil {
		t.Fatal("external schema reference accepted")
	}
	bad = req
	bad.Attachments = []agent.Attachment{{Kind: "file", Name: "../private", MIME: "text/plain", Data: []byte("test")}}
	if _, e := rt.Start(ctx, bad); e == nil {
		t.Fatal("path accepted")
	}
	if _, e := rt.StartCommand(ctx, "compare", nil, req); e == nil {
		t.Fatal("missing argument accepted")
	}
	h, e := rt.StartCommand(ctx, "compare", map[string]string{"topic": "sample"}, req)
	if e != nil {
		t.Fatal(e)
	}
	var result string
	var produced agent.Artifact
	for ev := range h.Events() {
		if ev.Kind == agent.EventArtifact {
			if err := json.Unmarshal(ev.Payload, &produced); err != nil {
				t.Fatal(err)
			}
		}
		if ev.Kind == agent.EventStructured {
			result = string(ev.Payload)
		}
	}
	if produced.Name != "report.txt" || produced.Size != len("tool evidence") {
		t.Fatalf("missing artifact event: %+v", produced)
	}
	if h.Status().Status != "completed" || result != `{"result":"saved"}` {
		t.Fatalf("result %q, %+v", result, h.Status())
	}
	if e := rt.Close(); e != nil {
		t.Fatal(e)
	}
	rt = open()
	rows, e := rt.SessionHistory(ctx, "local", "main", 0)
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) == 0 || len(rows[0].Attachments) != 3 || !strings.Contains(rows[0].Text, "compare sample") {
		t.Fatalf("history %#v", rows)
	}
	for i, info := range rows[0].Attachments {
		a, e := rt.ReadAttachment(ctx, "local", "main", info.Ref)
		if e != nil || !bytes.Equal(a.Data, req.Attachments[i].Data) || a.FileID != req.Attachments[i].FileID {
			t.Fatalf("restored part %d: %v", i, e)
		}
		if _, e := rt.ReadAttachment(ctx, "other", "main", info.Ref); e == nil {
			t.Fatal("cross-user attachment")
		}
	}
	store, e := rt.Artifacts("local", "main")
	if e != nil {
		t.Fatal(e)
	}
	meta, data, e := store.Read(ctx, "report.txt", nil)
	if e != nil || meta.Version != 0 || string(data) != "tool evidence" {
		t.Fatalf("tool artifact %+v %v", meta, e)
	}
	if _, e := store.Save(ctx, "user:profile", "text/plain", nil); e == nil {
		t.Fatal("cross-session alias accepted")
	}
	req.Attachments = nil
	req.SessionID = "invalid"
	req.Message = "INVALID_OUTPUT"
	h, e = rt.Start(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	for range h.Events() {
	}
	if h.Status().Status != "failed" {
		t.Fatal(h.Status())
	}
	req.SessionID = "unsupported"
	req.Message = "UNSUPPORTED_MEDIA"
	req.Output = nil
	req.Attachments = []agent.Attachment{{Kind: "file", Name: "a.pdf", MIME: "application/pdf", Data: []byte("%PDF-")}}
	h, e = rt.Start(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	var rejection bool
	for ev := range h.Events() {
		if ev.Kind == agent.EventError {
			rejection = true
		}
	}
	if !rejection || h.Status().Status != "failed" {
		t.Fatalf("provider refusal: event=%v status=%+v", rejection, h.Status())
	}
	req.Attachments = nil
	req.Output = &agent.OutputSchema{Name: "result", Schema: json.RawMessage(`{"type":"object"}`)}
	req.SessionID = "waiting"
	req.Message = "WAIT_HERE"
	h, e = rt.Start(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if e := h.Cancel(); e != nil {
		t.Fatal(e)
	}
	for range h.Events() {
	}
	if h.Status().Status != "cancelled" {
		t.Fatal(h.Status())
	}
	rt.Close()
	cfg.Workspace.Permission = agent.PermissionReadOnly
	rt = open()
	req.SessionID = "denied"
	req.Message = "deny"
	req.Output = nil
	h, e = rt.Start(ctx, req)
	if e != nil {
		t.Fatal(e)
	}
	for range h.Events() {
	}
	s, e := rt.Artifacts("local", "denied")
	if e != nil {
		t.Fatal(e)
	}
	if names, e := s.List(ctx); e != nil || len(names) != 0 {
		t.Fatal(names, e)
	}
	rt.Close()
	if e := mgr.Disable("fixture"); e != nil {
		t.Fatal(e)
	}
	rt = open()
	if len(rt.Definitions()) != 0 {
		t.Fatal("disabled definitions remain")
	}
	if _, e := rt.Start(ctx, req); e == nil {
		t.Fatal("disabled agent still selectable")
	}
}
