// Command extensions installs a local sample plugin and prints the
// components a runtime snapshot would assemble. It does not start an MCP
// process.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/plugin"
	trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: extensions <plugin-dir>")
		os.Exit(2)
	}
	root, err := os.MkdirTemp("", "grape-plugins-")
	if err != nil {
		fatal(err)
	}
	mgr, err := plugin.NewManager(filepath.Join(root, "installed"))
	if err != nil {
		fatal(err)
	}
	rec, err := mgr.InstallLocal(os.Args[1])
	if err != nil {
		fatal(err)
	}
	if err := mgr.Enable(rec.Name); err != nil {
		fatal(err)
	}
	cfg := config.Default()
	cfg.Plugins.Enabled = []string{rec.Name}
	rt, err := trpcruntime.New(trpcruntime.Options{
		Config:        &cfg,
		Model:         mockmodel.New("extensions", mockmodel.Echo),
		DataDir:       root,
		PluginManager: mgr,
	})
	if err != nil {
		fatal(err)
	}
	defer rt.Close()
	for _, comp := range rt.Plugins().Components {
		fmt.Printf("%s %s %s\n", comp.Kind, comp.Name, comp.Path)
	}
	for _, server := range rt.MCPServers() {
		fmt.Printf("mcp %s %s\n", server.Name, server.Transport)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
