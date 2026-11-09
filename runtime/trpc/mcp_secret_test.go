package trpcruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/plugin"
)

func TestMCPHeaderRefReachesServerAndStaysOutOfLogs(t *testing.T) {
	const token = "review-token-value-0123456789"
	t.Setenv("REVIEW_MCP_TOKEN", token)
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if auth := r.Header.Get("Authorization"); auth != "" {
			got = auth
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := config.Default()
	cfg.Journal.Enabled = true
	cfg.Journal.Path = filepath.Join(dir, "journal.ndjson")
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.MCP.Servers = []config.MCPServerConfig{{
		Name:          "remote",
		URL:           srv.URL + "/mcp",
		Transport:     "streamable",
		Headers:       map[string]string{"Authorization": "env:REVIEW_MCP_TOKEN"},
		CredentialRef: "env:REVIEW_MCP_TOKEN",
	}}
	rt, err := New(Options{Config: &cfg, Model: mockmodel.New("hdr", mockmodel.Echo), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	mu.Lock()
	auth := got
	mu.Unlock()
	if auth != token {
		t.Fatalf("server saw %q", auth)
	}
	for _, status := range rt.MCPStatus() {
		if strings.Contains(status.Detail, token) {
			t.Fatalf("status leaked the token: %+v", status)
		}
	}
	h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "hdr", Message: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(eventBlob(collect(t, h)), token) {
		t.Fatal("event stream leaked the token")
	}
	raw, err := os.ReadFile(cfg.Journal.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("journal leaked the token")
	}

	missing := config.Default()
	missing.Journal.Enabled = false
	missing.Model.Generation.Stream = boolPtr(false)
	missing.MCP.Servers = []config.MCPServerConfig{{
		Name: "gone", URL: srv.URL, Transport: "streamable",
		Headers: map[string]string{"Authorization": "env:DOES_NOT_EXIST_MCP"},
	}}
	miss, err := New(Options{Config: &missing, Model: mockmodel.New("miss", mockmodel.Echo), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer miss.Close()
	var detail string
	for _, status := range miss.MCPStatus() {
		if status.Name == "gone" {
			detail = status.Detail
		}
	}
	if !strings.Contains(detail, "missing env:DOES_NOT_EXIST_MCP") {
		t.Fatalf("detail %q", detail)
	}

	secretFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(secretFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileCfg := config.Default()
	fileCfg.Journal.Enabled = false
	fileCfg.Model.Generation.Stream = boolPtr(false)
	fileCfg.MCP.Servers = []config.MCPServerConfig{{
		Name: "file", URL: srv.URL + "/mcp", Transport: "streamable",
		CredentialRef: "file:" + secretFile,
	}}
	fileRT, err := New(Options{Config: &fileCfg, Model: mockmodel.New("file", mockmodel.Echo), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer fileRT.Close()
	mu.Lock()
	auth = got
	mu.Unlock()
	if auth != token {
		t.Fatalf("credential_ref saw %q", auth)
	}
}

func TestPluginMCPEnableAndClaudeHeader(t *testing.T) {
	const token = "plugin-token-value-0123456789"
	t.Setenv("REVIEW_MCP_TOKEN", token)
	var mu sync.Mutex
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if auth := r.Header.Get("Authorization"); auth != "" {
			got = auth
		}
		mu.Unlock()
		http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	root := t.TempDir()
	mgr, err := plugin.NewManager(filepath.Join(root, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")
	writeBundle(t, src, "bundle")
	body := `{"mcpServers":{"demo":{"url":"` + srv.URL + `/mcp","headers":{"Authorization":"${REVIEW_MCP_TOKEN}"}}}}`
	if err := os.WriteFile(filepath.Join(src, ".mcp.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(src); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("bundle"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	cfg.Plugins.Enabled = []string{"bundle"}
	off, err := New(Options{Config: &cfg, Model: mockmodel.New("off", mockmodel.Echo), DataDir: t.TempDir(), PluginManager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	for _, status := range off.MCPStatus() {
		if status.Name == "demo" {
			state = status.State
		}
	}
	off.Close()
	if state != "disabled" {
		t.Fatalf("unlisted plugin server state %q", state)
	}
	mu.Lock()
	if got != "" {
		mu.Unlock()
		t.Fatal("unlisted plugin server was dialed")
	}
	mu.Unlock()

	cfg.MCP.AllowPluginServers = []string{"bundle/demo"}
	on, err := New(Options{Config: &cfg, Model: mockmodel.New("on", mockmodel.Echo), DataDir: t.TempDir(), PluginManager: mgr})
	if err != nil {
		t.Fatal(err)
	}
	defer on.Close()
	mu.Lock()
	auth := got
	mu.Unlock()
	if auth != token {
		t.Fatalf("plugin header saw %q", auth)
	}
	for _, status := range on.MCPStatus() {
		if strings.Contains(status.Detail, token) {
			t.Fatalf("status leaked token: %+v", status)
		}
	}
}
