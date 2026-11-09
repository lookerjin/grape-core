package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// PluginServerGrant is one user authorization for a plugin MCP server.
// Name is "plugin/server". Trust "host" allows that stdio server to run
// on the host. A plugin cannot set this itself.
type PluginServerGrant struct {
	Name  string `yaml:"name" json:"name"`
	Trust string `yaml:"trust,omitempty" json:"trust,omitempty"`
}

// PluginGrants is the user allow list. YAML accepts a bare name or a map.
type PluginGrants []PluginServerGrant

// UnmarshalYAML accepts "plugin/server" or {name, trust} entries.
func (g *PluginGrants) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("allow_plugin_servers must be a list")
	}
	var out PluginGrants
	for _, item := range node.Content {
		switch item.Kind {
		case yaml.ScalarNode:
			out = append(out, PluginServerGrant{Name: item.Value})
		case yaml.MappingNode:
			var grant PluginServerGrant
			if err := item.Decode(&grant); err != nil {
				return err
			}
			if grant.Name == "" {
				return fmt.Errorf("allow_plugin_servers entry needs a name")
			}
			out = append(out, grant)
		default:
			return fmt.Errorf("allow_plugin_servers entry must be a name or a name/trust map")
		}
	}
	*g = out
	return nil
}

// Get returns the grant for plugin/server.
func (g PluginGrants) Get(name string) (PluginServerGrant, bool) {
	for _, item := range g {
		if item.Name == name {
			return item, true
		}
	}
	return PluginServerGrant{}, false
}
