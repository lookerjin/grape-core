package coding

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Resolve returns a cleaned absolute workspace directory.
func Resolve(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("workspace is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory")
	}
	return resolved, nil
}

// Within checks that rel stays inside root, including through symlinks.
// The check opens the path with os.Root. Callers that read or write must use
// Read, Save, or Replace, which perform the operation on that same root.
func Within(root, rel string) (string, error) {
	root, err := Resolve(root)
	if err != nil {
		return "", err
	}
	rel, err = cleanRel(rel)
	if err != nil {
		return "", err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	if rel == "." {
		return root, nil
	}
	f, err := r.Open(rel)
	if err != nil {
		return "", err
	}
	f.Close()
	return filepath.Join(root, rel), nil
}

func cleanRel(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return ".", nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q is outside the workspace", rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return cleaned, nil
}

func openWorkspace(root string) (string, *os.Root, error) {
	root, err := Resolve(root)
	if err != nil {
		return "", nil, err
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", nil, err
	}
	return root, r, nil
}

// Read returns the text of a workspace file. Symlinks that leave the
// workspace, absolute symlinks, and dangling symlinks are errors.
func Read(root, rel string) (string, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return "", err
	}
	_, r, err := openWorkspace(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	info, err := r.Stat(rel)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("is a directory")
	}
	b, err := r.ReadFile(rel)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("refusing to read non-UTF-8 file %s", rel)
	}
	return string(b), nil
}

// List returns names of entries in a workspace directory.
func List(root, rel string) ([]string, error) {
	rel, err := cleanRel(rel)
	if err != nil {
		return nil, err
	}
	_, r, err := openWorkspace(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	entries, err := fs.ReadDir(r.FS(), rel)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.Name())
	}
	return out, nil
}

// Hit is one substring match.
type Hit struct {
	Path string
	Line int
	Text string
}

// Search walks the workspace for a literal substring. It skips directories
// named .git and does not follow symlinks that leave the workspace.
func Search(root, substr string) ([]Hit, error) {
	if substr == "" {
		return nil, fmt.Errorf("empty search")
	}
	_, r, err := openWorkspace(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var hits []Hit
	err = fs.WalkDir(r.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		b, err := r.ReadFile(path)
		if err != nil || !utf8.Valid(b) {
			return nil
		}
		text := string(b)
		if !strings.Contains(text, substr) {
			return nil
		}
		line := 1
		for _, chunk := range strings.Split(text, "\n") {
			if strings.Contains(chunk, substr) {
				hits = append(hits, Hit{Path: path, Line: line, Text: chunk})
			}
			line++
		}
		return nil
	})
	return hits, err
}

// Replace replaces old with new in a workspace file and keeps the file mode.
// n is the maximum number of replacements. A non-positive n means one.
// The read and write both go through os.Root, so a symlink cannot retarget
// the write outside the workspace between the two steps.
func Replace(root, rel, old, new string, n int) error {
	if old == "" {
		return fmt.Errorf("empty old string")
	}
	rel, err := cleanRel(rel)
	if err != nil {
		return err
	}
	_, r, err := openWorkspace(root)
	if err != nil {
		return err
	}
	defer r.Close()
	info, err := r.Stat(rel)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory")
	}
	b, err := r.ReadFile(rel)
	if err != nil {
		return err
	}
	if !utf8.Valid(b) {
		return fmt.Errorf("refusing to edit non-UTF-8 file %s", rel)
	}
	if n <= 0 {
		n = 1
	}
	text := string(b)
	if strings.Count(text, old) == 0 {
		return fmt.Errorf("old string not found")
	}
	updated := strings.Replace(text, old, new, n)
	mode := info.Mode().Perm()
	if err := r.WriteFile(rel, []byte(updated), mode); err != nil {
		return err
	}
	return r.Chmod(rel, mode)
}

// Save writes a UTF-8 file inside the workspace.
func Save(root, rel, contents string, overwrite bool) error {
	if !utf8.ValidString(contents) {
		return fmt.Errorf("refusing to write non-UTF-8 content")
	}
	rel, err := cleanRel(rel)
	if err != nil {
		return err
	}
	_, r, err := openWorkspace(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if dir := filepath.Dir(rel); dir != "." {
		if err := r.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if !overwrite {
		if _, err := r.Lstat(rel); err == nil {
			return fmt.Errorf("file exists")
		}
	}
	return r.WriteFile(rel, []byte(contents), 0o644)
}

// Match lists files and directories under rel whose path or base name matches
// pattern. An empty pattern matches everything. Symlinks are not followed.
func Match(root, rel, pattern string) (files, dirs []string, err error) {
	rel, err = cleanRel(rel)
	if err != nil {
		return nil, nil, err
	}
	_, r, err := openWorkspace(root)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close()
	if _, err := fs.Stat(r.FS(), rel); err != nil {
		return nil, nil, err
	}
	err = fs.WalkDir(r.FS(), rel, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == rel && d.IsDir() {
			return nil
		}
		if pattern != "" {
			baseOK, _ := filepath.Match(pattern, d.Name())
			pathOK, _ := filepath.Match(pattern, path)
			if !baseOK && !pathOK {
				return nil
			}
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, dirs, err
}
