package skill

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverOrderAndConflict(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "a", "skills", "review"), "review", "from a")
	writeSkill(t, filepath.Join(root, "b", "skills", "other"), "other", "from b")
	got, err := Discover("error", filepath.Join(root, "a"), filepath.Join(root, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "other" || got[1].Name != "review" {
		t.Fatalf("%#v", got)
	}
	writeSkill(t, filepath.Join(root, "b", "skills", "review"), "review", "from b")
	if _, err := Discover("error", filepath.Join(root, "a"), filepath.Join(root, "b")); err == nil {
		t.Fatal("expected conflict")
	}
	latest, err := Discover("latest", filepath.Join(root, "a"), filepath.Join(root, "b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range latest {
		if item.Name == "review" && item.Description != "from b" {
			t.Fatalf("%#v", item)
		}
	}
}

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\nBody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
