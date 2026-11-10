package trpcruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lookerjin/grape-core/skill"
	trpcskill "trpc.group/trpc-go/trpc-agent-go/skill"
)

// skillSnapshot is the Discover result. Get and Path read that selection.
// They do not scan again. tRPC FSRepository keeps the first name and skips
// a file that fails to parse. Discover returns those as errors.
type skillSnapshot struct {
	roots []string
	order []skill.Summary
	byDir map[string]skill.Summary
}

func newSkillSnapshot(policy string, roots []string) (trpcskill.Repository, error) {
	found, err := skill.Discover(policy, roots...)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	snap := &skillSnapshot{
		order: found,
		byDir: map[string]skill.Summary{},
	}
	seenRoot := map[string]struct{}{}
	for _, item := range found {
		snap.byDir[item.Name] = item
		if _, ok := seenRoot[item.Root]; ok {
			continue
		}
		seenRoot[item.Root] = struct{}{}
		snap.roots = append(snap.roots, item.Root)
	}
	return snap, nil
}

func (s *skillSnapshot) Summaries() []trpcskill.Summary {
	if s == nil {
		return nil
	}
	out := make([]trpcskill.Summary, 0, len(s.order))
	for _, item := range s.order {
		out = append(out, trpcskill.Summary{Name: item.Name, Description: item.Description})
	}
	return out
}

func (s *skillSnapshot) Path(name string) (string, error) {
	item, ok := s.byDir[name]
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	return item.Dir, nil
}

func (s *skillSnapshot) Roots() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.roots...)
}

func (s *skillSnapshot) Get(name string) (*trpcskill.Skill, error) {
	item, ok := s.byDir[name]
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	body, err := skillBody(item.Dir)
	if err != nil {
		return nil, err
	}
	docs, err := skillDocs(item.Dir)
	if err != nil {
		return nil, err
	}
	return &trpcskill.Skill{
		Summary: trpcskill.Summary{Name: item.Name, Description: item.Description},
		Body:    body,
		Docs:    docs,
	}, nil
}

func skillBody(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return "", err
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---") {
		return text, nil
	}
	rest := strings.TrimPrefix(text, "---")
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		return text, nil
	}
	return strings.TrimPrefix(rest[idx+4:], "\n"), nil
}

func skillDocs(dir string) ([]trpcskill.Doc, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var docs []trpcskill.Doc
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "SKILL.md" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		docs = append(docs, trpcskill.Doc{Path: entry.Name(), Content: string(raw)})
	}
	return docs, nil
}
