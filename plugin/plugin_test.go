package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRejectsSymlinkEscapeAndMissingManifest(t *testing.T) {
	root := t.TempDir()
	mgr, err := NewManager(filepath.Join(root, "installed"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "bad-link")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	writePlugin(t, src, "linked")
	outside := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "leak")); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(src); err == nil {
		t.Fatal("expected symlink rejection")
	}
	if _, err := os.Stat(filepath.Join(mgr.Root(), "linked")); !os.IsNotExist(err) {
		t.Fatal("failed install left a plugin directory")
	}
	broken := filepath.Join(root, "no-manifest")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(broken); err == nil {
		t.Fatal("expected manifest error")
	}
}

func TestValidateRel(t *testing.T) {
	if err := ValidateRel("../x"); err == nil {
		t.Fatal("expected error")
	}
	if err := ValidateRel("skills/demo"); err != nil {
		t.Fatal(err)
	}
}

func TestInstallEnableResolveAndGitRevision(t *testing.T) {
	root := t.TempDir()
	mgr, err := NewManager(filepath.Join(root, "installed"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")
	writePlugin(t, src, "demo")
	rec, err := mgr.InstallLocal(src)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Enabled || rec.Fingerprint == "" {
		t.Fatalf("%+v", rec)
	}
	if err := mgr.Enable("demo"); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Inspect("demo")
	if err != nil || !got.Enabled {
		t.Fatal(err, got.Enabled)
	}
	resolved, err := Resolve([]Record{got}, "error")
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Components) < 4 {
		t.Fatalf("components %#v", resolved.Components)
	}
	gitSrc := filepath.Join(root, "git-src")
	writePlugin(t, gitSrc, "fromgit")
	run(t, gitSrc, "git", "init")
	run(t, gitSrc, "git", "config", "user.email", "test@example.com")
	run(t, gitSrc, "git", "config", "user.name", "test")
	run(t, gitSrc, "git", "add", ".")
	run(t, gitSrc, "git", "commit", "-m", "init")
	rev := strings.TrimSpace(runOut(t, gitSrc, "git", "rev-parse", "HEAD"))
	installed, err := mgr.InstallGit(context.Background(), gitSrc, rev)
	if err != nil {
		t.Fatal(err)
	}
	if installed.Revision != rev {
		t.Fatalf("revision %q want %q", installed.Revision, rev)
	}
	if err := mgr.Uninstall("demo"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Inspect("demo"); err == nil {
		t.Fatal("expected uninstall")
	}
}

func TestConflictPolicy(t *testing.T) {
	a := Record{Name: "a", Enabled: true, Components: []Component{{Kind: KindSkill, Name: "same", Path: "a"}}}
	b := Record{Name: "b", Enabled: true, Components: []Component{{Kind: KindSkill, Name: "same", Path: "b"}}}
	if _, err := Resolve([]Record{a, b}, "error"); err == nil {
		t.Fatal("expected conflict")
	}
	got, err := Resolve([]Record{a, b}, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Components) != 1 || got.Components[0].Path != "b" {
		t.Fatalf("%#v", got.Components)
	}
}

func writePlugin(t *testing.T, dir, name string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755))
	must(os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(`{"name":"`+name+`","version":"0.1.0","description":"demo"}`), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "skills", "review"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "skills", "review", "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\nBe careful.\n"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "hooks"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), []byte(`{"hooks":[{"event":"BeforeTool","command":"echo","args":["hi"],"blocking":true}]}`), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "rules"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "rules", "style.md"), []byte("Use short functions.\n"), 0o644))
	must(os.MkdirAll(filepath.Join(dir, "commands"), 0o755))
	must(os.WriteFile(filepath.Join(dir, "commands", "review.md"), []byte("Review the diff.\n"), 0o644))
	must(os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"demo":{"command":"echo","args":["hi"]}}}`), 0o644))
}

func run(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func runOut(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return string(out)
}
