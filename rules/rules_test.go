package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrderAndSummary(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "global")
	work := filepath.Join(root, "work")
	plug := filepath.Join(root, "plug")
	for _, dir := range []string{global, work, plug} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(global, "a.md"), []byte("global rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "b.md"), []byte("---\nglobs: [\"*.go\"]\n---\nworkspace rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plug, "c.md"), []byte("plugin rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(Source{"global", global}, Source{"workspace", work}, Source{"plugin", plug})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Source != "global" || got[1].Source != "workspace" || got[2].Source != "plugin" {
		t.Fatalf("%#v", got)
	}
	applied := Applicable(got, "main.go")
	summary := Summary(applied)
	if !strings.Contains(summary, "global rule") || !strings.Contains(summary, "workspace rule") || !strings.Contains(summary, "plugin rule") {
		t.Fatalf("%s", summary)
	}
	skipped := Applicable(got, "notes.txt")
	text := Summary(skipped)
	if strings.Contains(text, "workspace rule") {
		t.Fatalf("glob matched notes: %s", text)
	}
}
