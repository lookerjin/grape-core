package coding

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathBoundaryAndReplaceKeepsMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("alpha\nbeta\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := Within(root, "../secret.txt"); err == nil {
		t.Fatal("expected escape rejection")
	}
	if _, err := Read(root, "link"); err == nil {
		t.Fatal("expected symlink rejection")
	}
	if err := Replace(root, "a.txt", "alpha", "ALPHA", 1); err != nil {
		t.Fatal(err)
	}
	got, err := Read(root, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ALPHA\nbeta\n" {
		t.Fatalf("content %q", got)
	}
	info, err := os.Stat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	hits, err := Search(root, "beta")
	if err != nil || len(hits) != 1 || hits[0].Path != "a.txt" {
		t.Fatalf("hits %#v err %v", hits, err)
	}
}

func TestGitStatusDoesNotStage(t *testing.T) {
	root := t.TempDir()
	gitCmd(t, root, "init")
	gitCmd(t, root, "config", "user.email", "test@example.com")
	gitCmd(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "add", "keep.txt")
	gitCmd(t, root, "commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "other.txt"), []byte("leave\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := GitStatus(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "keep.txt") || !strings.Contains(status, "other.txt") {
		t.Fatalf("status %q", status)
	}
	for _, line := range strings.Split(strings.TrimRight(status, "\n"), "\n") {
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '?' {
			t.Fatalf("line was staged: %q", line)
		}
	}
	diff, err := GitDiff(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "-one") || !strings.Contains(diff, "+two") {
		t.Fatalf("diff %s", diff)
	}
	again, err := GitStatus(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if again != status {
		t.Fatalf("status changed after inspection\n%s\n%s", status, again)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
