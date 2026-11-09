package plugin

import (
	"context"
	"errors"
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

func TestInstallRollbackAndFailedUpdate(t *testing.T) {
	root := t.TempDir()
	mgr, err := NewManager(filepath.Join(root, "installed"))
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")
	writePlugin(t, src, "demo")
	if err := os.WriteFile(filepath.Join(src, "VERSION"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	copied := 0
	failCopy = func(string) error {
		copied++
		if copied > 1 {
			return errors.New("injected copy failure")
		}
		return nil
	}
	t.Cleanup(func() { failCopy = nil })
	if _, err := mgr.InstallLocal(src); err == nil {
		t.Fatal("expected mid-copy failure")
	}
	if _, err := os.Stat(filepath.Join(mgr.Root(), "demo")); !os.IsNotExist(err) {
		t.Fatal("partial install directory remains")
	}
	entries, err := os.ReadDir(mgr.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-") || entry.Name() == "demo" {
			t.Fatalf("leftover %s", entry.Name())
		}
	}
	if raw, err := os.ReadFile(filepath.Join(mgr.Root(), "installed.json")); err == nil && strings.Contains(string(raw), "demo") {
		t.Fatalf("install record written: %s", raw)
	}
	failCopy = nil
	if _, err := mgr.InstallLocal(src); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("demo"); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(root, "next")
	writePlugin(t, next, "demo")
	if err := os.WriteFile(filepath.Join(next, ".claude-plugin", "plugin.json"), []byte(`{"name":"demo","version":"9.9.9"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	copied = 0
	failCopy = func(string) error {
		copied++
		if copied > 1 {
			return errors.New("injected copy failure")
		}
		return nil
	}
	if _, err := mgr.UpdateLocal(next); err == nil {
		t.Fatal("expected failed update")
	}
	got, err := mgr.Inspect("demo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.1.0" || !got.Enabled {
		t.Fatalf("previous install not preserved: %+v", got)
	}
	body, err := os.ReadFile(filepath.Join(mgr.Root(), "demo", "VERSION"))
	if err != nil || string(body) != "1\n" {
		t.Fatalf("version file %q err %v", body, err)
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

func TestPluginComponentNamesDoNotCollide(t *testing.T) {
	root := t.TempDir()
	mgr, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(root, "a-src")
	b := filepath.Join(root, "b-src")
	writePlugin(t, a, "alpha")
	writePlugin(t, b, "beta")
	if err := os.RemoveAll(filepath.Join(b, "skills")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b, "rules")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(b, "commands")); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(a); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.InstallLocal(b); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("alpha"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Enable("beta"); err != nil {
		t.Fatal(err)
	}
	records := []Record{}
	for _, name := range []string{"alpha", "beta"} {
		rec, err := mgr.Inspect(name)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, rec)
	}
	resolved, err := Resolve(records, "error")
	if err != nil {
		t.Fatal(err)
	}
	var sawAlpha, sawBeta bool
	for _, comp := range resolved.Components {
		if comp.Kind == KindHook && comp.Name == "alpha/hooks" {
			sawAlpha = true
		}
		if comp.Kind == KindMCP && comp.Name == "beta/demo" {
			sawBeta = true
		}
	}
	if !sawAlpha || !sawBeta {
		t.Fatalf("components %#v", resolved.Components)
	}
	if len(resolved.DiscoveryOnly) != 2 || resolved.DiscoveryOnly[0] != KindCommand || resolved.DiscoveryOnly[1] != KindAgent {
		t.Fatalf("discovery %#v", resolved.DiscoveryOnly)
	}
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
