package trpcruntime

import (
	"fmt"

	"github.com/lookerjin/grape-core/skill"
	trpcskill "trpc.group/trpc-go/trpc-agent-go/skill"
)

// skillSnapshot records which directory Discover selected for each name.
// Get and Path load that directory through tRPC's FSRepository. They do not
// parse SKILL.md again to decide the winner. tRPC's own scan, given every
// root at once, keeps the first name and skips a file that fails to parse.
// Discover returns those as errors before this snapshot is built.
type skillSnapshot struct {
	roots []string
	order []skill.Summary
	repos map[string]*trpcskill.FSRepository
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
		repos: map[string]*trpcskill.FSRepository{},
	}
	seenRoot := map[string]struct{}{}
	for _, item := range found {
		// The selected skill directory is a repository root. tRPC indexes
		// the SKILL.md in that directory and loads its docs from there.
		repo, err := trpcskill.NewFSRepository(item.Dir)
		if err != nil {
			return nil, err
		}
		if _, err := repo.Get(item.Name); err != nil {
			return nil, fmt.Errorf("skill %q from %s: %w", item.Name, item.Dir, err)
		}
		snap.repos[item.Name] = repo
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
	repo, ok := s.repos[name]
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	return repo.Path(name)
}

func (s *skillSnapshot) Roots() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.roots...)
}

func (s *skillSnapshot) Get(name string) (*trpcskill.Skill, error) {
	repo, ok := s.repos[name]
	if !ok {
		return nil, fmt.Errorf("skill %q not found", name)
	}
	return repo.Get(name)
}
