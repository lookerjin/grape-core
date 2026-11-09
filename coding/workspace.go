package coding

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// Within resolves rel against root and rejects paths that escape it,
// including symlink targets outside the workspace.
func Within(root, rel string) (string, error) {
	root, err := Resolve(root)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" || rel == "." {
		return root, nil
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute path %q is outside the workspace API", rel)
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace")
	}
	full := filepath.Join(root, cleaned)
	parent := filepath.Dir(full)
	if err := walkExisting(root, parent); err != nil {
		return "", err
	}
	if info, err := os.Lstat(full); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := filepath.EvalSymlinks(full)
		if err != nil {
			return "", err
		}
		if !inside(root, target) {
			return "", fmt.Errorf("symlink escapes workspace")
		}
		return target, nil
	}
	return full, nil
}

func walkExisting(root, path string) error {
	path = filepath.Clean(path)
	if !inside(root, path) && path != root {
		return fmt.Errorf("path escapes workspace")
	}
	current := root
	rest, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	if rest == "." {
		return nil
	}
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(current)
			if err != nil {
				return err
			}
			if !inside(root, target) {
				return fmt.Errorf("symlink escapes workspace")
			}
		}
	}
	return nil
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Read returns the text of a workspace file.
func Read(root, rel string) (string, error) {
	full, err := Within(root, rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("is a directory")
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// List returns names of entries in a workspace directory.
func List(root, rel string) ([]string, error) {
	full, err := Within(root, rel)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
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
// named .git.
func Search(root, substr string) ([]Hit, error) {
	if substr == "" {
		return nil, fmt.Errorf("empty search")
	}
	root, err := Resolve(root)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(b)
		if !strings.Contains(text, substr) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		line := 1
		for _, chunk := range strings.Split(text, "\n") {
			if strings.Contains(chunk, substr) {
				hits = append(hits, Hit{Path: rel, Line: line, Text: chunk})
			}
			line++
		}
		return nil
	})
	return hits, err
}

// Replace replaces old with new in a workspace file and keeps the file mode.
// n is the maximum number of replacements. A non-positive n means one.
func Replace(root, rel, old, new string, n int) error {
	if old == "" {
		return fmt.Errorf("empty old string")
	}
	full, err := Within(root, rel)
	if err != nil {
		return err
	}
	info, err := os.Stat(full)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory")
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	if n <= 0 {
		n = 1
	}
	text := string(b)
	count := strings.Count(text, old)
	if count == 0 {
		return fmt.Errorf("old string not found")
	}
	updated := strings.Replace(text, old, new, n)
	return os.WriteFile(full, []byte(updated), info.Mode().Perm())
}
