// Command basic runs one grape-core turn.
//
// Without GRAPE_REAL_MODEL=1 it uses a network-free mock model and prints
// the event stream. Set GRAPE_REAL_MODEL=1 plus MODEL_API_KEY and MODEL_BASE_URL
// to call an OpenAI-compatible endpoint. No API key is written to disk.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg := config.Default()
	if len(os.Args) > 1 {
		loaded, err := config.Load(os.Args[1])
		if err != nil {
			return err
		}
		cfg = loaded
	}
	cfg.Model.APIKeyEnv = envOr(cfg.Model.APIKeyEnv, "MODEL_API_KEY")
	cfg.Model.BaseURL = envOr(cfg.Model.BaseURL, os.Getenv("MODEL_BASE_URL"))
	cfg.Model.Name = envOr(cfg.Model.Name, "example-model")
	message := envOr(os.Getenv("GRAPE_MESSAGE"), "hello")

	opts := trpcruntime.Options{Config: &cfg}
	if os.Getenv("GRAPE_REAL_MODEL") != "1" {
		opts.Model = mockmodel.New("example", func(ctx context.Context, req *model.Request) (*model.Response, error) {
			return &model.Response{
				Choices: []model.Choice{{
					Message: model.NewAssistantMessage("ack: " + message),
				}},
			}, nil
		}).WithStream()
	}
	rt, err := trpcruntime.New(opts)
	if err != nil {
		return err
	}
	defer rt.Close()

	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	handle, err := rt.Start(runCtx, agent.RunRequest{
		UserID:    "local-user",
		SessionID: "demo",
		Workspace: ".",
		Message:   message,
	})
	if err != nil {
		return err
	}
	for evt := range handle.Events() {
		fmt.Printf("%d %s %s\n", evt.Sequence, evt.Kind, evt.Payload)
	}
	return nil
}

func envOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
