package trpcruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionRestoresAcrossProcesses(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sessionturn")
	build := exec.Command("go", "build", "-o", bin, "./internal/cmd/sessionturn")
	build.Dir = moduleRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=1", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	data := t.TempDir()
	first := runTurn(t, bin, data, "first-turn")
	if !strings.Contains(first, "USER first-turn") {
		t.Fatalf("first process: %s", first)
	}
	second := runTurn(t, bin, data, "second-turn")
	if !strings.Contains(second, "USER first-turn") || !strings.Contains(second, "USER second-turn") {
		t.Fatalf("second process did not restore the session: %s", second)
	}
}

func runTurn(t *testing.T, bin, data, message string) string {
	t.Helper()
	cmd := exec.Command(bin, "-data", data, "-message", message)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1", "GOTOOLCHAIN=local")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sessionturn %s: %v\n%s", message, err, out)
	}
	return string(out)
}
