package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kind identifies a discovered component.
type Kind string

const (
	KindSkill   Kind = "skill"
	KindMCP     Kind = "mcp"
	KindHook    Kind = "hook"
	KindRule    Kind = "rule"
	KindCommand Kind = "command"
	KindAgent   Kind = "agent"
)

// Component is one file or directory inside an installed bundle.
type Component struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// Manifest is the supported subset of a Claude-style plugin.json.
// Other JSON fields are ignored and are not treated as aliases.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
	License     string `json:"license"`
}

// Record is the on-disk install state.
type Record struct {
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Description string      `json:"description,omitempty"`
	Source      string      `json:"source"`
	Revision    string      `json:"revision,omitempty"`
	Fingerprint string      `json:"fingerprint"`
	Enabled     bool        `json:"enabled"`
	Components  []Component `json:"components"`
	InstalledAt time.Time   `json:"installed_at"`
}

// Resolved is a deterministic component list for one runtime snapshot.
type Resolved struct {
	Plugins    []Record
	Components []Component
}

// ErrConflict is returned when two components share a kind and name.
var ErrConflict = errors.New("component conflict")

// Manager stores installs under root.
type Manager struct {
	root string
}

// NewManager uses root as the install directory. It is created on demand.
func NewManager(root string) (*Manager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("plugin root is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Manager{root: root}, nil
}

// Root returns the install directory.
func (m *Manager) Root() string { return m.root }

// InstallLocal copies a directory bundle into the manager root.
func (m *Manager) InstallLocal(src string) (*Record, error) {
	return m.installSource(src, "")
}

// UpdateLocal replaces an installed bundle from a directory.
// A failure while copying or validating leaves the previous install in place.
func (m *Manager) UpdateLocal(src string) (*Record, error) {
	return m.installSource(src, "")
}

func (m *Manager) installSource(src, revision string) (*Record, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugin source is not a directory")
	}
	return m.installTree(abs, abs, revision)
}

// List returns installed records sorted by name.
func (m *Manager) List() ([]Record, error) {
	state, err := m.load()
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(state))
	for _, rec := range state {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Inspect returns one installed record.
func (m *Manager) Inspect(name string) (Record, error) {
	state, err := m.load()
	if err != nil {
		return Record{}, err
	}
	rec, ok := state[name]
	if !ok {
		return Record{}, fmt.Errorf("plugin %q is not installed", name)
	}
	return rec, nil
}

// Enable marks a plugin enabled for future runtime snapshots.
func (m *Manager) Enable(name string) error { return m.setEnabled(name, true) }

// Disable marks a plugin disabled for future runtime snapshots.
func (m *Manager) Disable(name string) error { return m.setEnabled(name, false) }

func (m *Manager) setEnabled(name string, enabled bool) error {
	state, err := m.load()
	if err != nil {
		return err
	}
	rec, ok := state[name]
	if !ok {
		return fmt.Errorf("plugin %q is not installed", name)
	}
	rec.Enabled = enabled
	state[name] = rec
	return m.store(state)
}

// Uninstall removes an installed plugin and its record.
func (m *Manager) Uninstall(name string) error {
	state, err := m.load()
	if err != nil {
		return err
	}
	rec, ok := state[name]
	if !ok {
		return fmt.Errorf("plugin %q is not installed", name)
	}
	if err := os.RemoveAll(filepath.Join(m.root, rec.Name)); err != nil {
		return err
	}
	delete(state, name)
	return m.store(state)
}

// Resolve returns enabled plugins. policy is error, earliest, or latest.
func Resolve(records []Record, policy string) (Resolved, error) {
	if policy == "" {
		policy = "error"
	}
	var enabled []Record
	for _, rec := range records {
		if rec.Enabled {
			enabled = append(enabled, rec)
		}
	}
	sort.Slice(enabled, func(i, j int) bool { return enabled[i].Name < enabled[j].Name })
	seen := map[string]Component{}
	var components []Component
	for _, rec := range enabled {
		comps := append([]Component(nil), rec.Components...)
		sort.Slice(comps, func(i, j int) bool {
			if comps[i].Kind == comps[j].Kind {
				return comps[i].Name < comps[j].Name
			}
			return comps[i].Kind < comps[j].Kind
		})
		for _, comp := range comps {
			key := string(comp.Kind) + "\x00" + comp.Name
			prev, ok := seen[key]
			if !ok {
				seen[key] = comp
				components = append(components, comp)
				continue
			}
			switch policy {
			case "error":
				return Resolved{}, fmt.Errorf("%w: %s %s from %s and %s", ErrConflict, comp.Kind, comp.Name, prev.Path, comp.Path)
			case "earliest":
				continue
			case "latest":
				seen[key] = comp
				for i := range components {
					if string(components[i].Kind)+"\x00"+components[i].Name == key {
						components[i] = comp
					}
				}
			default:
				return Resolved{}, fmt.Errorf("unknown conflict policy %q", policy)
			}
		}
	}
	sort.Slice(components, func(i, j int) bool {
		if components[i].Kind == components[j].Kind {
			return components[i].Name < components[j].Name
		}
		return components[i].Kind < components[j].Kind
	})
	return Resolved{Plugins: enabled, Components: components}, nil
}

func (m *Manager) installTree(src, sourceLabel, revision string) (*Record, error) {
	staging, err := os.MkdirTemp(m.root, ".staging-")
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := copyTree(src, staging); err != nil {
		return nil, err
	}
	manifest, err := ReadManifest(staging)
	if err != nil {
		return nil, err
	}
	components, err := Discover(staging)
	if err != nil {
		return nil, err
	}
	sum, err := fingerprint(staging)
	if err != nil {
		return nil, err
	}
	final := filepath.Join(m.root, manifest.Name)
	backup := final + ".bak"
	_ = os.RemoveAll(backup)
	hadFinal := false
	if _, err := os.Stat(final); err == nil {
		hadFinal = true
		if err := os.Rename(final, backup); err != nil {
			return nil, err
		}
	}
	if err := os.Rename(staging, final); err != nil {
		if hadFinal {
			_ = os.Rename(backup, final)
		}
		return nil, err
	}
	committed = true
	_ = os.RemoveAll(backup)
	rec := Record{
		Name:        manifest.Name,
		Version:     manifest.Version,
		Description: manifest.Description,
		Source:      sourceLabel,
		Revision:    revision,
		Fingerprint: sum,
		Enabled:     false,
		Components:  rewritePaths(components, staging, final),
		InstalledAt: time.Now().UTC(),
	}
	state, err := m.load()
	if err != nil {
		_ = os.RemoveAll(final)
		return nil, err
	}
	if prev, ok := state[rec.Name]; ok {
		rec.Enabled = prev.Enabled
	}
	state[rec.Name] = rec
	if err := m.store(state); err != nil {
		delete(state, rec.Name)
		_ = os.RemoveAll(final)
		return nil, err
	}
	return &rec, nil
}

func rewritePaths(components []Component, staging, final string) []Component {
	out := make([]Component, len(components))
	for i, comp := range components {
		if rel, err := filepath.Rel(staging, comp.Path); err == nil {
			comp.Path = filepath.Join(final, rel)
		}
		out[i] = comp
	}
	return out
}

func (m *Manager) statePath() string { return filepath.Join(m.root, "installed.json") }

func (m *Manager) load() (map[string]Record, error) {
	raw, err := os.ReadFile(m.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Record{}, nil
		}
		return nil, err
	}
	var state map[string]Record
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state == nil {
		state = map[string]Record{}
	}
	return state, nil
}

func (m *Manager) store(state map[string]Record) error {
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath())
}

// ReadManifest reads .claude-plugin/plugin.json.
func ReadManifest(root string) (Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("plugin manifest: %w", err)
	}
	manifest.Name = strings.TrimSpace(manifest.Name)
	if manifest.Name == "" {
		return Manifest{}, fmt.Errorf("plugin manifest name is empty")
	}
	if strings.Contains(manifest.Name, "/") || strings.Contains(manifest.Name, `\`) || manifest.Name == "." || manifest.Name == ".." {
		return Manifest{}, fmt.Errorf("plugin name %q is not a single path segment", manifest.Name)
	}
	return manifest, nil
}

// Discover lists components using the directory convention.
func Discover(root string) ([]Component, error) {
	var out []Component
	skillRoot := filepath.Join(root, "skills")
	if entries, err := os.ReadDir(skillRoot); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			skillFile := filepath.Join(skillRoot, entry.Name(), "SKILL.md")
			if _, err := os.Stat(skillFile); err != nil {
				continue
			}
			out = append(out, Component{Kind: KindSkill, Name: entry.Name(), Path: filepath.Dir(skillFile)})
		}
	}
	label := pluginLabel(root)
	mcpFile := filepath.Join(root, ".mcp.json")
	if _, err := os.Stat(mcpFile); err == nil {
		for _, server := range mcpServerNames(mcpFile) {
			out = append(out, Component{Kind: KindMCP, Name: label + "/" + server, Path: mcpFile})
		}
	}
	if _, err := os.Stat(filepath.Join(root, "hooks", "hooks.json")); err == nil {
		out = append(out, Component{Kind: KindHook, Name: label + "/hooks", Path: filepath.Join(root, "hooks", "hooks.json")})
	}
	rulesDir := filepath.Join(root, "rules")
	if entries, err := os.ReadDir(rulesDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			out = append(out, Component{Kind: KindRule, Name: strings.TrimSuffix(entry.Name(), ".md"), Path: filepath.Join(rulesDir, entry.Name())})
		}
	}
	out = append(out, markdownComponents(filepath.Join(root, "commands"), KindCommand)...)
	out = append(out, markdownComponents(filepath.Join(root, "agents"), KindAgent)...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind == out[j].Kind {
			return out[i].Name < out[j].Name
		}
		return out[i].Kind < out[j].Kind
	})
	return out, nil
}

func pluginLabel(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if err != nil {
		return filepath.Base(root)
	}
	var doc struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &doc) != nil || strings.TrimSpace(doc.Name) == "" {
		return filepath.Base(root)
	}
	return strings.TrimSpace(doc.Name)
}

func mcpServerNames(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{"mcp"}
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.MCPServers) == 0 {
		return []string{"mcp"}
	}
	names := make([]string, 0, len(doc.MCPServers))
	for name := range doc.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func markdownComponents(dir string, kind Kind) []Component {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Component
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		out = append(out, Component{
			Kind: kind,
			Name: strings.TrimSuffix(entry.Name(), ".md"),
			Path: filepath.Join(dir, entry.Name()),
		})
	}
	return out
}

// ValidateRel rejects a relative path that escapes its root.
func ValidateRel(rel string) error {
	rel = filepath.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("path %q escapes the plugin root", rel)
	}
	return nil
}

func copyTree(src, dst string) error {
	srcRoot, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if err := ValidateRel(rel); err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("symlink %s: %w", rel, err)
			}
			if !inside(srcRoot, resolved) {
				return fmt.Errorf("symlink %s escapes the plugin source", rel)
			}
			return fmt.Errorf("symlink %s is not installed", rel)
		}
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// failCopy is a test seam. Production leaves it nil.
var failCopy func(dst string) error

func copyFile(src, dst string, mode os.FileMode) error {
	if failCopy != nil {
		if err := failCopy(dst); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func fingerprint(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		if _, err := io.WriteString(h, rel); err != nil {
			return "", err
		}
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
