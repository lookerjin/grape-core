package trpcruntime

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/coding"
)

func (rt *Runtime) openExtraRoots() error {
	rt.extraRoots = map[string]*coding.Directory{}
	for _, path := range rt.cfg.Workspace.ExtraRoots {
		if !filepath.IsAbs(path) || path != filepath.Clean(path) {
			return agent.Invalid("workspace.extra_roots", fmt.Errorf("expected a clean absolute directory"))
		}
		if rt.extraRoots[path] != nil {
			continue
		}
		d, e := coding.OpenDirectory(path)
		if e != nil {
			return agent.Invalid("workspace.extra_roots", e)
		}
		rt.extraRoots[path] = d
	}
	return nil
}

func (rt *Runtime) fileRoot(ctx context.Context, fallback, root string) (*coding.Directory, func(), error) {
	if root == "" {
		d, e := coding.OpenDirectory(rt.workspaceFrom(ctx, fallback))
		if e != nil {
			return nil, nil, e
		}
		return d, func() { d.Close() }, nil
	}
	d := rt.extraRoots[root]
	if d == nil {
		return nil, nil, agent.Wrap(agent.ErrPermissionDenied, "root", fmt.Errorf("ungranted directory"))
	}
	return d, func() {}, nil
}
