package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSensitivePathsKeepsOnlyExistingDefaults(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte("machine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(t.TempDir(), "grape-core")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(session, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(key, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := SensitivePaths(DenyRequest{
		Home:            home,
		DataDir:         data,
		SessionPath:     session,
		CredentialFiles: []string{"file:" + key, "file:" + filepath.Join(t.TempDir(), "missing")},
		Mode:            "add",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		filepath.Join(home, ".ssh"):   true,
		filepath.Join(home, ".netrc"): true,
		data:                          true,
		session:                       true,
		key:                           true,
	}
	if len(got.Paths) != len(want) {
		t.Fatalf("paths %#v", got.Paths)
	}
	for _, path := range got.Paths {
		if !want[path] {
			t.Fatalf("unexpected %s in %#v", path, got.Paths)
		}
	}
	if len(got.Globs) != 0 {
		t.Fatalf("globs %#v", got.Globs)
	}
}

func TestSensitivePathsOverrideAndGlob(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(t.TempDir(), "extra")
	if err := os.Mkdir(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := SensitivePaths(DenyRequest{
		Home:  home,
		Mode:  "override",
		Extra: []string{extra, "secrets/*.pem"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Paths) != 1 || got.Paths[0] != extra {
		t.Fatalf("paths %#v", got.Paths)
	}
	if len(got.Globs) != 1 || got.Globs[0] != "secrets/*.pem" {
		t.Fatalf("globs %#v", got.Globs)
	}
	if _, err := SensitivePaths(DenyRequest{Mode: "override", Extra: []string{"/tmp/*.pem"}}); err == nil {
		t.Fatal("expected absolute glob rejection")
	}
}
