// Command coding runs the offline fixture task from testdata/fixture.
//
// It copies the fixture into a temporary directory, asks the mock model to
// fix the failing test, and prints git status. Set GRAPE_REAL_MODEL=1 to use
// the configured model instead of the mock. Real model calls are not part of
// the default path.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	trpcruntime "github.com/lookerjin/grape-core/runtime/trpc"
	"github.com/lookerjin/grape-core/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ws, err := copyFixture()
	if err != nil {
		return err
	}
	fmt.Println("workspace", ws)
	cfg := config.Default()
	cfg.Workspace.Root = ws
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Model.APIKeyEnv = "MODEL_API_KEY"
	cfg.Model.Name = envOr(os.Getenv("MODEL_NAME"), "example-model")
	cfg.Model.BaseURL = os.Getenv("MODEL_BASE_URL")
	opts := trpcruntime.Options{Config: &cfg}
	if os.Getenv("GRAPE_REAL_MODEL") != "1" {
		opts.Model = mockmodel.New("coding", mockmodel.CodingFix)
	}
	rt, err := trpcruntime.New(opts)
	if err != nil {
		return err
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	handle, err := rt.Start(ctx, agent.RunRequest{
		UserID:    "local-user",
		SessionID: "coding-demo",
		Workspace: ws,
		Message:   "fix the failing test",
	})
	if err != nil {
		return err
	}
	for evt := range handle.Events() {
		fmt.Printf("%d %s %s\n", evt.Sequence, evt.Kind, evt.Payload)
	}
	body, err := os.ReadFile(filepath.Join(ws, "math.go"))
	if err != nil {
		return err
	}
	fmt.Printf("math.go:\n%s", body)
	if _, err := os.Stat(filepath.Join(ws, ".git")); err == nil {
		diff, err := exec.Command("git", "-C", ws, "diff", "--", "math.go").CombinedOutput()
		if err != nil {
			return fmt.Errorf("git diff: %w\n%s", err, diff)
		}
		fmt.Printf("diff:\n%s", diff)
	}
	return nil
}

func copyFixture() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	src := filepath.Join(wd, "testdata", "fixture")
	if _, err := os.Stat(src); err != nil {
		src = filepath.Join(wd, "..", "..", "testdata", "fixture")
	}
	dst, err := os.MkdirTemp("", "grape-coding-")
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		in, err := os.Open(filepath.Join(src, entry.Name()))
		if err != nil {
			return "", err
		}
		out, err := os.Create(filepath.Join(dst, entry.Name()))
		if err != nil {
			in.Close()
			return "", err
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			return "", err
		}
	}
	return dst, nil
}

func envOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
