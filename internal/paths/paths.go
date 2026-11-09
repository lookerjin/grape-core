// Package paths resolves grape-core data directories.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DataDir is the default root for sessions, plugins, and memory.
//
// On Windows this is %AppData%\grape-core. On other systems it is
// ~/.grape-core. The Windows layout is selected from GOOS; it has not been
// executed on a Windows machine in this repository's Linux verification.
func DataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	configDir := ""
	if runtime.GOOS == "windows" {
		configDir, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return dataDir(runtime.GOOS, home, configDir)
}

func dataDir(goos, home, configDir string) (string, error) {
	if goos == "windows" {
		if strings.TrimSpace(configDir) == "" {
			return "", fmt.Errorf("windows config dir is empty")
		}
		return filepath.Join(configDir, "grape-core"), nil
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("home dir is empty")
	}
	return filepath.Join(home, ".grape-core"), nil
}

// Expand cleans a path and expands a leading ~ to the user home directory.
func Expand(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	return filepath.Clean(path), nil
}
