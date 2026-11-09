package paths

import (
	"path/filepath"
	"testing"
)

func TestDataDirWindowsUsesConfigDir(t *testing.T) {
	configDir := `C:\Users\looker\AppData\Roaming`
	got, err := dataDir("windows", `C:\Users\looker`, configDir)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configDir, "grape-core")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestDataDirUnixUsesHome(t *testing.T) {
	got, err := dataDir("linux", "/home/looker", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/looker/.grape-core" {
		t.Fatalf("got %q", got)
	}
}

func TestDataDirRejectsEmpty(t *testing.T) {
	if _, err := dataDir("windows", "", ""); err == nil {
		t.Fatal("expected error")
	}
	if _, err := dataDir("linux", "", ""); err == nil {
		t.Fatal("expected error")
	}
}
