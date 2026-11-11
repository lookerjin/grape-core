package trpcruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/hooks"
	"github.com/lookerjin/grape-core/mcp"
	"github.com/lookerjin/grape-core/plugin"
	"github.com/lookerjin/grape-core/rules"
	trpcskill "trpc.group/trpc-go/trpc-agent-go/skill"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func (rt *Runtime) collectExtensions(mgr *plugin.Manager, dataDir string, grants map[string][]ToolSpec) error {
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
				records[i].Enabled = records[i].Enabled && allow[records[i].Name]
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
				if !hookAllowed(rt.cfg.Hooks.Allow, comp.Name) {
					continue
				}
				specs, err := hooks.Load(comp.Path)
				if err != nil {
					return err
				}
				hookSpecs = append(hookSpecs, specs...)
			case plugin.KindCommand, plugin.KindAgent:
				d, err := plugin.ParseDefinition(comp)
				if err != nil {
					return fmt.Errorf("%w: %s: %v", agent.ErrPlugin, comp.Name, err)
				}
				if d.Kind == plugin.KindAgent {
					if err := rt.preparePluginAgent(d, grants); err != nil {
						return err
					}
				}
				rt.definitions = append(rt.definitions, d)
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
					server.PluginKey = comp.Name
					if grant, ok := rt.cfg.MCP.AllowPluginServers.Get(comp.Name); ok {
						server.Enabled = true
						if grant.Trust == "host" {
							server.Trust = "host"
						}
					}
					rt.mcpServers = append(rt.mcpServers, server)
				}
			}
		}
		rt.pluginHooks = hookSpecs
	}
	hookSpecs := append([]hooks.Spec{}, rt.pluginHooks...)
	hookSpecs = append(hookSpecs, rt.grantedHooks...)
	if len(hookSpecs) > 0 {
		timeout, err := config.Duration(rt.cfg.Hooks.Timeout)
		if err != nil {
			return err
		}
		rt.hookRunner = hooks.NewRunner(rt.exec, hookSpecs, rt.cfg.Hooks.MaxConcurrent, timeout)
	}
	repo, err := newSkillSnapshot(rt.cfg.Plugins.Conflict, rt.skillRoots)
	if err != nil {
		return err
	}
	rt.skills = repo
	text, err := rt.ruleTextFor(rt.cfg.Workspace.Root)
	if err != nil {
		return err
	}
	rt.ruleText = text
	return nil
}

func (rt *Runtime) preparePluginAgent(d plugin.Definition, grants map[string][]ToolSpec) error {
	if rt.fixed != nil || d.Name == rootAgentName || len(rt.nested[d.Name]) > 0 {
		return fmt.Errorf("%w: incompatible plugin agent %q", agent.ErrPlugin, d.Name)
	}
	for _, s := range rt.subagents {
		if s.Name == d.Name {
			return fmt.Errorf("%w: conflicting agent %q", agent.ErrPlugin, d.Name)
		}
	}
	for _, t := range rt.trustedTools {
		if t.Declaration().Name == d.Name {
			return fmt.Errorf("%w: conflicting agent %q", agent.ErrPlugin, d.Name)
		}
	}
	if rt.pluginTools == nil {
		rt.pluginTools = make(map[string][]tool.Tool)
	}
	for _, name := range d.Tools {
		var selected []ToolSpec
		for _, spec := range grants[d.Plugin+"/"+d.Name] {
			if spec.Tool != nil && spec.Tool.Declaration() != nil && spec.Tool.Declaration().Name == name && !spec.Trusted {
				selected = append(selected, spec)
			}
		}
		if len(selected) != 1 {
			return fmt.Errorf("%w: agent %s requires a unique grant for %s", agent.ErrPlugin, d.Name, name)
		}
		if err := prepareTools(nil, selected); err != nil {
			return err
		}
		rt.pluginTools[d.Name] = append(rt.pluginTools[d.Name], selected[0].Tool)
	}
	return nil
}
func hookAllowed(allow []string, name string) bool {
	for _, item := range allow {
		if item == name {
			return true
		}
	}
	return false
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

func (rt *Runtime) skillRepository() (trpcskill.Repository, error) {
	if rt == nil {
		return nil, nil
	}
	return rt.skills, nil
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

// Definitions lists executable Markdown components from this runtime's snapshot.
func (rt *Runtime) Definitions() []plugin.Definition {
	out := slices.Clone(rt.definitions)
	for i := range out {
		out[i].Arguments = slices.Clone(out[i].Arguments)
		out[i].Tools = slices.Clone(out[i].Tools)
	}
	return out
}

// StartCommand renders a named template into an ordinary user turn.
func (rt *Runtime) StartCommand(ctx context.Context, name string, args map[string]string, req agent.RunRequest) (agent.RunHandle, error) {
	var command *plugin.Definition
	for i := range rt.definitions {
		d := &rt.definitions[i]
		if d.Kind == plugin.KindCommand && d.Name == name {
			command = d
			break
		}
	}
	if command == nil {
		return nil, fmt.Errorf("%w: unknown command %q", agent.ErrPlugin, name)
	}
	if req.Message != "" {
		return nil, agent.Invalid("command", fmt.Errorf("message supplied twice"))
	}
	msg, err := command.Render(args)
	if err != nil {
		return nil, agent.Invalid("command", err)
	}
	req.Message = msg
	return rt.Start(ctx, req)
}
