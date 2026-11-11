// Command models exercises the public model infrastructure without a UI or native SDK imports.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/model"
	"github.com/lookerjin/grape-core/model/catalog"
	grape "github.com/lookerjin/grape-core/runtime/trpc"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	dir := os.Getenv("GRAPE_MODEL_DATA")
	if dir == "" {
		base, e := os.UserConfigDir()
		if e != nil {
			return e
		}
		dir = filepath.Join(base, "grape-model-example")
	}
	if len(args) == 0 {
		args = []string{"catalog"}
	}
	switch args[0] {
	case "catalog", "refresh":
		c, e := catalog.Open(catalog.Options{CachePath: filepath.Join(dir, "catalog.json"), SourcesPath: os.Getenv("GRAPE_CATALOG_SOURCES"), Offline: args[0] == "catalog"})
		if e != nil {
			return e
		}
		if args[0] == "refresh" {
			if _, e = c.Refresh(ctx); e != nil {
				return e
			}
		}
		s := c.Status()
		fmt.Printf("%s  %s  %d models  %s\n", s.Origin, s.SourceID, s.RecordCount, s.Digest)
		if s.Warning != "" {
			fmt.Println(s.Warning)
		}
		for i, m := range c.List() {
			if i == 8 {
				break
			}
			fmt.Println(m.Provider, m.ID)
		}
		return nil
	}
	scope := model.Scope{App: "grape-model-example", User: "local"}
	opts := model.Options{Scope: scope, Database: filepath.Join(dir, "connections.db")}
	if id := os.Getenv("HF_CLIENT_ID"); id != "" {
		opts.OAuth = []model.OAuthProvider{model.HuggingFace(id, os.Getenv("HF_CLIENT_SECRET"))}
	}
	r, e := model.Open(opts)
	if e != nil {
		return e
	}
	defer r.Close()
	switch args[0] {
	case "add-env":
		if len(args) != 4 {
			return errors.New("add-env HTTPS_BASE_URL KEY_ENV MODEL")
		}
		c, e := r.AddEnv(ctx, model.Provider{ID: "custom", Protocol: model.Chat, Endpoint: args[1]}, "API key", args[2])
		if e != nil {
			return e
		}
		return r.SetDefault(ctx, model.Ref{ConnectionID: c.ID, ModelID: args[3]})
	case "device", "login":
		if len(args) != 2 {
			return errors.New("login or device requires a model ID")
		}
		var l *model.Login
		if args[0] == "device" {
			l, e = r.BeginDevice(ctx, "huggingface-router")
		} else {
			l, e = r.BeginLogin(ctx, "huggingface-router", os.Getenv("HF_CALLBACK"))
		}
		if e != nil {
			return e
		}
		defer l.Cancel()
		fmt.Println(l.Challenge.URL, l.Challenge.UserCode)
		c, e := l.Complete("Hugging Face")
		if e != nil {
			return e
		}
		return r.SetDefault(ctx, model.Ref{ConnectionID: c.ID, ModelID: args[1]})
	case "accounts":
		list, e := r.List(ctx)
		if e != nil {
			return e
		}
		for _, c := range list {
			fmt.Println(c.ID, c.Label, c.Provider.ID)
		}
		return nil
	case "logout":
		if len(args) != 2 {
			return errors.New("logout requires a connection ID")
		}
		return r.Remove(ctx, args[1])
	case "run":
		if len(args) != 2 {
			return errors.New("run requires a prompt")
		}
		cfg := config.Default()
		cfg.Workspace.Permission = agent.PermissionReadOnly
		rt, e := grape.New(grape.Options{AppName: scope.App, Config: &cfg, ModelRegistry: r, DataDir: dir})
		if e != nil {
			return e
		}
		defer rt.Close()
		h, e := rt.Start(ctx, agent.RunRequest{UserID: scope.User, SessionID: "sample", Message: args[1]})
		if e != nil {
			return e
		}
		for ev := range h.Events() {
			fmt.Println(ev.Kind, string(ev.Payload))
		}
		if status := h.Status(); status.Status != "completed" {
			return fmt.Errorf("run %s: %s", status.Status, status.Error)
		}
		return nil
	default:
		return errors.New("use catalog, refresh, add-env, login, device, accounts, run or logout")
	}
}
