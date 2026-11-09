// Package plugin installs and records product extension bundles.
//
// A bundle may contain skills, MCP config, hooks, rules, commands, and agent
// templates. This package does not execute them and does not import those
// packages. The runtime assembles the component list it returns.
package plugin
