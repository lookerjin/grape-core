package trpcruntime

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/hooks"
	"github.com/lookerjin/grape-core/mcp"
	"github.com/lookerjin/grape-core/plugin"
	"github.com/lookerjin/grape-core/rules"
	"github.com/lookerjin/grape-core/skill"
	trpcskill "trpc.group/trpc-go/trpc-agent-go/skill"
)

func (rt *Runtime) collectExtensions(mgr *plugin.Manager, dataDir string) error {
	configured, err := serversFromConfig(rt.cfg)
	if err != nil {
		return err
	}
	rt.mcpServers = append(rt.mcpServers, configured...)
	rt.globalRules = filepath.Join(dataDir, "rules")
	for _, path := range rt.cfg.Skills.Paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		rt.skillRoots = append(rt.skillRoots, path)
	}
	if mgr != nil {
		records, err := mgr.List()
		if err != nil {
			return err
		}
		if len(rt.cfg.Plugins.Enabled) > 0 {
			allow := map[string]bool{}
			for _, name := range rt.cfg.Plugins.Enabled {
				allow[name] = true
			}
			for i := range records {
				records[i].Enabled = allow[records[i].Name]
			}
		}
		resolved, err := plugin.Resolve(records, rt.cfg.Plugins.Conflict)
		if err != nil {
			return err
		}
		rt.resolved = resolved
		var hookSpecs []hooks.Spec
		for _, comp := range resolved.Components {
			switch comp.Kind {
			case plugin.KindSkill:
				rt.skillRoots = append(rt.skillRoots, comp.Path)
			case plugin.KindRule:
				rt.ruleSources = append(rt.ruleSources, rules.Source{Name: "plugin:" + comp.Name, Path: comp.Path})
			case plugin.KindHook:
				specs, err := hooks.Load(comp.Path)
				if err != nil {
					return err
				}
				hookSpecs = append(hookSpecs, specs...)
			case plugin.KindMCP:
				raw, err := os.ReadFile(comp.Path)
				if err != nil {
					return err
				}
				servers, err := mcp.ParseClaudeConfig(raw, comp.Path)
				if err != nil {
					return err
				}
				serverName := comp.Name
				if i := strings.LastIndex(comp.Name, "/"); i >= 0 {
					serverName = comp.Name[i+1:]
				}
				for _, server := range servers {
					if server.Name != serverName {
						continue
					}
					if mcp.PluginAllowed(rt.cfg.MCP.AllowPluginServers, comp.Name) {
						server.Enabled = true
					}
					rt.mcpServers = append(rt.mcpServers, server)
				}
			}
		}
		if len(hookSpecs) > 0 {
			timeout, err := config.Duration(rt.cfg.Hooks.Timeout)
			if err != nil {
				return err
			}
			rt.hookRunner = hooks.NewRunner(rt.exec, hookSpecs, rt.cfg.Hooks.MaxConcurrent, timeout)
		}
	}
	if _, err := skill.Discover(rt.cfg.Plugins.Conflict, rt.skillRoots...); err != nil {
		return err
	}
	text, err := rt.ruleTextFor(rt.cfg.Workspace.Root)
	if err != nil {
		return err
	}
	rt.ruleText = text
	return nil
}

func (rt *Runtime) ruleTextFor(workspace string) (string, error) {
	sources := []rules.Source{{Name: "global", Path: rt.globalRules}}
	sources = append(sources, rt.ruleSources...)
	if workspace != "" {
		sources = append(sources, rules.Source{Name: "workspace", Path: filepath.Join(workspace, ".grape", "rules")})
	}
	loaded, err := rules.Load(sources...)
	if err != nil {
		return "", err
	}
	return rules.Summary(loaded), nil
}

func (rt *Runtime) skillRepository() (*trpcskill.FSRepository, error) {
	var roots []string
	for _, root := range rt.skillRoots {
		if _, err := os.Stat(root); err == nil {
			roots = append(roots, root)
		}
	}
	if len(roots) == 0 {
		return nil, nil
	}
	return trpcskill.NewFSRepository(roots...)
}

// Plugins returns the component snapshot captured when the runtime was built.
func (rt *Runtime) Plugins() plugin.Resolved { return rt.resolved }

// MCPServers returns the redacted MCP servers captured for this runtime.
func (rt *Runtime) MCPServers() []mcp.Server {
	out := make([]mcp.Server, len(rt.mcpServers))
	copy(out, rt.mcpServers)
	for i := range out {
		if len(out[i].Headers) == 0 {
			continue
		}
		masked := make(map[string]string, len(out[i].Headers))
		for key := range out[i].Headers {
			masked[key] = "***"
		}
		out[i].Headers = masked
	}
	return out
}
