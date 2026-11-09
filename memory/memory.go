package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one recalled note.
type Entry struct {
	Path    string
	Snippet string
}

// Store is a directory of Markdown notes.
type Store struct {
	root string
}

// Open creates the standard directories if they are missing.
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("memory root is empty")
	}
	for _, dir := range []string{"", "practices", "projects", "references"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			return nil, err
		}
	}
	store := &Store{root: root}
	if err := store.rebuildIndex(); err != nil {
		return nil, err
	}
	return store, nil
}

// Root returns the memory directory.
func (s *Store) Root() string { return s.root }

// Put writes a note under a relative Markdown path and refreshes the index.
func (s *Store) Put(rel, body string) error {
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("memory path escapes the root")
	}
	if !strings.HasSuffix(rel, ".md") || rel == "MEMORY.md" {
		return fmt.Errorf("memory notes must be markdown files other than MEMORY.md")
	}
	full := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		return err
	}
	return s.rebuildIndex()
}

// Read returns one note.
func (s *Store) Read(rel string) (string, error) {
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("memory path escapes the root")
	}
	b, err := os.ReadFile(filepath.Join(s.root, rel))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Recall returns notes whose path or body contains query. Matching is
// case-insensitive. An empty query returns no entries.
func (s *Store) Recall(query string) ([]Entry, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, nil
	}
	var hits []Entry
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || d.Name() == "MEMORY.md" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.root, path)
		text := string(b)
		if !strings.Contains(strings.ToLower(rel), query) && !strings.Contains(strings.ToLower(text), query) {
			return nil
		}
		hits = append(hits, Entry{Path: rel, Snippet: snippet(text, query)})
		return nil
	})
	sort.Slice(hits, func(i, j int) bool { return hits[i].Path < hits[j].Path })
	return hits, err
}

func (s *Store) rebuildIndex() error {
	var lines []string
	lines = append(lines, "# Memory", "", "Updated: "+time.Now().UTC().Format(time.RFC3339), "")
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || d.Name() == "MEMORY.md" {
			return err
		}
		rel, _ := filepath.Rel(s.root, path)
		lines = append(lines, "- "+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(lines[4:])
	return os.WriteFile(filepath.Join(s.root, "MEMORY.md"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func snippet(text, query string) string {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, query)
	if idx < 0 {
		if len(text) > 160 {
			return text[:160]
		}
		return text
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := idx + len(query) + 80
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}
