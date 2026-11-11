// Command extensions inspects a local plugin and optionally runs a named
// Markdown agent or command using the offline echo model.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"github.com/lookerjin/grape-core/plugin"
	trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
)

func main() {
	if len(os.Args) != 2 && len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: extensions <plugin-dir> [agent <name> <message> | command <name> <JSON-object>]")
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
	for _, d := range rt.Definitions() {
		fmt.Printf("executable %s %s: %s\n", d.Kind, d.Name, d.Description)
	}
	for _, server := range rt.MCPServers() {
		fmt.Printf("mcp %s %s\n", server.Name, server.Transport)
	}
	if len(os.Args) == 5 {
		req := agent.RunRequest{UserID: "local", SessionID: "extensions"}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var handle agent.RunHandle
		switch os.Args[2] {
		case "agent":
			req.AgentID = os.Args[3]
			req.Message = os.Args[4]
			handle, err = rt.Start(ctx, req)
		case "command":
			var args map[string]string
			if err = json.Unmarshal([]byte(os.Args[4]), &args); err == nil {
				handle, err = rt.StartCommand(ctx, os.Args[3], args, req)
			}
		default:
			err = fmt.Errorf("unknown operation %q", os.Args[2])
		}
		if err != nil {
			fatal(err)
		}
		for e := range handle.Events() {
			fmt.Printf("%s %s\n", e.Kind, e.Payload)
		}
		if handle.Status().Status != "completed" {
			fatal(fmt.Errorf("run ended: %+v", handle.Status()))
		}
	}

}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
