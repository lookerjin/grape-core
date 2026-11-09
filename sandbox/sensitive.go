package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultSensitiveRelPaths are home-relative locations masked when they exist.
// The list is the default for sandbox.deny_read mode "add".
var DefaultSensitiveRelPaths = []string{
	".ssh",
	".gnupg",
	".aws",
	".azure",
	filepath.Join(".config", "gcloud"),
	".kube",
	filepath.Join(".docker", "config.json"),
	".netrc",
	".git-credentials",
	filepath.Join(".config", "gh"),
}

// DenyRequest is the input for the managed no-access list.
type DenyRequest struct {
	// Home is the user home used to expand ~ and the default relative paths.
	Home string
	// DataDir is the grape-core data directory. It is masked when it exists
	// and does not contain Workspace.
	DataDir string
	// SessionPath is the SQLite session file. It is masked when it exists
	// outside DataDir and Workspace.
	SessionPath string
	// Extra is sandbox.deny_read. Entries containing a glob metacharacter
	// are workspace-relative WithNoAccessGlobs patterns. Other entries are
	// host paths.
	Extra []string
	// Mode is "add" (default) or "override". Override uses only Extra.
	Mode string
	// CredentialFiles are existing files named by config, usually file: refs.
	CredentialFiles []string
	// Workspace is the command workspace. Paths that contain it are omitted
	// so the workspace grant is not covered by a deny mask.
	Workspace string
}

// DenyList is what a managed profile will actually pass to tRPC.
type DenyList struct {
	Paths []string
	Globs []string
}

// SensitivePaths builds the no-access list. Only host paths that exist are
// returned. Missing defaults are omitted because v1.12.0 does not mask a
// missing absolute path that sits outside a writable mount. Globs must be
// workspace-relative; absolute globs are rejected.
func SensitivePaths(req DenyRequest) (DenyList, error) {
	mode := req.Mode
	if mode == "" {
		mode = "add"
	}
	if mode != "add" && mode != "override" {
		return DenyList{}, fmt.Errorf("sandbox.deny_read_mode %q", mode)
	}
	var candidates []string
	if mode == "add" {
		candidates = append(candidates, defaultCandidates(req.Home)...)
		candidates = append(candidates, req.DataDir, req.SessionPath)
		candidates = append(candidates, req.CredentialFiles...)
	}
	candidates = append(candidates, req.Extra...)
	workspace := cleanExisting(req.Workspace)
	seenPath := map[string]struct{}{}
	seenGlob := map[string]struct{}{}
	var paths, globs []string
	for _, raw := range candidates {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if isGlob(raw) {
			glob, err := normalizeGlob(raw)
			if err != nil {
				return DenyList{}, err
			}
			if _, ok := seenGlob[glob]; ok {
				continue
			}
			seenGlob[glob] = struct{}{}
			globs = append(globs, glob)
			continue
		}
		path, ok := existingPath(req.Home, raw)
		if !ok || coversWorkspace(path, workspace) {
			continue
		}
		if _, ok := seenPath[path]; ok {
			continue
		}
		seenPath[path] = struct{}{}
		paths = append(paths, path)
	}
	paths = dropNested(paths)
	sort.Strings(paths)
	sort.Strings(globs)
	return DenyList{Paths: paths, Globs: globs}, nil
}

func defaultCandidates(home string) []string {
	if strings.TrimSpace(home) == "" {
		return nil
	}
	out := make([]string, 0, len(DefaultSensitiveRelPaths))
	for _, rel := range DefaultSensitiveRelPaths {
		out = append(out, filepath.Join(home, rel))
	}
	return out
}

func isGlob(raw string) bool {
	return strings.ContainsAny(raw, "*?[")
}

func normalizeGlob(raw string) (string, error) {
	raw = strings.TrimPrefix(raw, "file:")
	if strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) || filepath.IsAbs(raw) {
		return "", fmt.Errorf("sandbox.deny_read glob %q must be workspace-relative; v1.12.0 rejects absolute no-access globs on Linux", raw)
	}
	glob := filepath.ToSlash(filepath.Clean(raw))
	if glob == "." || glob == ".." || strings.HasPrefix(glob, "../") {
		return "", fmt.Errorf("sandbox.deny_read glob %q escapes the workspace", raw)
	}
	return glob, nil
}

func existingPath(home, raw string) (string, bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "file:")
	if raw == "" {
		return "", false
	}
	if raw == "~" {
		raw = home
	} else if strings.HasPrefix(raw, "~/") || strings.HasPrefix(raw, `~\`) {
		if home == "" {
			return "", false
		}
		raw = filepath.Join(home, raw[2:])
	}
	if !filepath.IsAbs(raw) {
		return "", false
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", false
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err != nil {
		return "", false
	}
	return abs, true
}

func cleanExisting(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return filepath.Clean(abs)
}

func coversWorkspace(path, workspace string) bool {
	if workspace == "" || path == "" {
		return false
	}
	if path == workspace {
		return true
	}
	rel, err := filepath.Rel(path, workspace)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func dropNested(paths []string) []string {
	sort.Strings(paths)
	var out []string
	for _, path := range paths {
		nested := false
		for _, kept := range out {
			rel, err := filepath.Rel(kept, path)
			if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, path)
		}
	}
	return out
}
