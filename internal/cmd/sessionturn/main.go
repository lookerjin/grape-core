// Command sessionturn runs one grape-core turn and prints the user messages
// the model received. Tests start it twice against the same data directory.
package main

import (
	"context"
	"flag"
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
	data := flag.String("data", "", "data directory")
	sessionID := flag.String("session", "persist", "session id")
	userID := flag.String("user", "u", "user id")
	message := flag.String("message", "", "user message")
	flag.Parse()
	if *data == "" || *message == "" {
		fmt.Fprintln(os.Stderr, "data and message are required")
		os.Exit(2)
	}
	cfg := config.Default()
	stream := false
	cfg.Model.Generation.Stream = &stream
	cfg.Journal.Enabled = false
	mdl := mockmodel.New("turn", mockmodel.Echo)
	rt, err := trpcruntime.New(trpcruntime.Options{Config: &cfg, Model: mdl, DataDir: *data})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	handle, err := rt.Start(ctx, agent.RunRequest{
		UserID: *userID, SessionID: *sessionID, Message: *message,
	})
	if err != nil {
		_ = rt.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for evt := range handle.Events() {
		if evt.Terminal() && evt.Kind != agent.EventCompleted {
			fmt.Fprintf(os.Stderr, "terminal %s %s\n", evt.Kind, evt.Payload)
			_ = rt.Close()
			os.Exit(1)
		}
	}
	for _, req := range mdl.Requests() {
		if req == nil {
			continue
		}
		for _, msg := range req.Messages {
			if msg.Role == model.RoleUser && msg.Content != "" {
				fmt.Println("USER " + msg.Content)
			}
		}
	}
	if err := rt.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
