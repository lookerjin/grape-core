package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
)

func TestLoadMergePriorityAndExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.yaml")
	workspace := filepath.Join(dir, "workspace.yaml")
	if err := os.WriteFile(user, []byte(`
model:
  name: user-model
  timeout: 10s
memory:
  enabled: true
context:
  max_llm_calls: 4
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workspace, []byte(`
model:
  timeout: 2s
workspace:
  permission: workspace_write
memory:
  enabled: false
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(user, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.Name != "user-model" {
		t.Fatalf("name %q", cfg.Model.Name)
	}
	if cfg.Model.Timeout != "2s" {
		t.Fatalf("timeout %q", cfg.Model.Timeout)
	}
	if cfg.Workspace.Permission != "workspace_write" {
		t.Fatalf("permission %q", cfg.Workspace.Permission)
	}
	if cfg.Memory.Enabled {
		t.Fatal("explicit false was lost")
	}
	if cfg.Context.MaxLLMCalls != 4 {
		t.Fatalf("max calls %d", cfg.Context.MaxLLMCalls)
	}
	if cfg.Evolution.Enabled {
		t.Fatal("evolution defaulted on")
	}
	if !cfg.KnowledgeOnly() {
		t.Fatal("knowledge only default")
	}
	if cfg.Runtime.Provider != "trpc" {
		t.Fatalf("provider %q", cfg.Runtime.Provider)
	}
}

func TestLoadExpandsEnv(t *testing.T) {
	t.Setenv("MODEL_BASE_URL", "http://127.0.0.1:9/v1")
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("model:\n  base_url: ${MODEL_BASE_URL}\n  name: ${MISSING:-demo}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model.BaseURL != "http://127.0.0.1:9/v1" {
		t.Fatalf("base %q", cfg.Model.BaseURL)
	}
	if cfg.Model.Name != "demo" {
		t.Fatalf("name %q", cfg.Model.Name)
	}
}

func TestLoadUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(path, []byte("model:\n  not_a_field: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !agent.AsKind(err, agent.ErrInvalidConfig) {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadRejectsRawHeaderSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	body := "mcp:\n  servers:\n    - name: demo\n      headers:\n        Authorization: sk-secret\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !agent.AsKind(err, agent.ErrInvalidConfig) {
		t.Fatalf("err=%v", err)
	}
}

func TestRedactedYAMLRemovesKeyMaterial(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "sk-test-secret-value")
	cfg := Default()
	cfg.Model.APIKeyEnv = "MODEL_API_KEY"
	cfg.Model.BaseURL = "http://example.test/?key=sk-test-secret-value"
	cfg.MCP.Servers = []MCPServerConfig{{
		Name:    "demo",
		Headers: map[string]string{"Authorization": "env:MODEL_API_KEY"},
	}}
	out := cfg.RedactedYAML()
	if strings.Contains(out, "sk-test-secret-value") {
		t.Fatalf("secret leaked: %s", out)
	}
	if strings.Contains(out, "env:MODEL_API_KEY") && !strings.Contains(out, "***") {
		t.Fatalf("header not masked: %s", out)
	}
}

func TestValidateExternalBackendRequired(t *testing.T) {
	cfg := Default()
	cfg.Sandbox.Mode = "external"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error")
	}
}

func TestDefaultMemoryAndEvolutionOff(t *testing.T) {
	cfg := Default()
	if cfg.Memory.Enabled || cfg.Evolution.Enabled {
		t.Fatalf("%+v", cfg)
	}
}
