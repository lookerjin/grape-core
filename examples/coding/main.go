// Command coding runs the offline fixture task from testdata/fixture.
//
// It copies the fixture into a temporary directory, asks the mock model to
// fix the failing test, and checks the command result. Set GRAPE_REAL_MODEL=1 to use
// the configured model instead of the mock. Real model calls are not part of
// the default path.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	defer os.RemoveAll(ws)
	data, err := os.MkdirTemp("", "grape-coding-session-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(data)
	fmt.Println("workspace", ws)
	cfg := config.Default()
	cfg.Workspace.Root = ws
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Sandbox.Mode = sandbox.ModeManaged
	cfg.Model.APIKeyEnv = "MODEL_API_KEY"
	cfg.Model.Name = envOr(os.Getenv("MODEL_NAME"), "example-model")
	cfg.Model.BaseURL = os.Getenv("MODEL_BASE_URL")
	cfg.Context.TotalMaxLLMCalls = config.Int(12)
	opts := trpcruntime.Options{Config: &cfg, DataDir: data, Subagents: []trpcruntime.SubagentSpec{{Name: "reviewer", Instruction: "Review the supplied test output."}}}
	if os.Getenv("GRAPE_REAL_MODEL") != "1" {
		opts.Model = mockmodel.New("coding", mockmodel.CodingFix)
		opts.Subagents[0].Model = mockmodel.New("reviewer", mockmodel.Echo)
		if os.Getenv("GRAPE_FIXTURE_HOST_TEST") == "1" {
			cfg.Sandbox.Mode = sandbox.ModeDangerFullAccess
		}
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
		Message:   "Fix the failing test, run go test ./..., and ask reviewer to inspect the test result.",
	})
	if err != nil {
		return err
	}
	var verified, reviewed bool
	for evt := range handle.Events() {
		fmt.Printf("%d %s %s\n", evt.Sequence, evt.Kind, evt.Payload)
		if evt.Kind != agent.EventToolEnd {
			continue
		}
		var tool agent.ToolPayload
		if err := json.Unmarshal(evt.Payload, &tool); err != nil {
			return err
		}
		var content string
		if err := json.Unmarshal(tool.Result, &content); err != nil {
			return err
		}
		switch tool.Name {
		case "run_command":
			var result struct {
				ExitCode *int   `json:"exit_code"`
				Stdout   string `json:"stdout"`
			}
			verified = json.Unmarshal([]byte(content), &result) == nil && result.ExitCode != nil && *result.ExitCode == 0 && strings.Contains(result.Stdout, "example.com/fixture")
		case "reviewer":
			reviewed = tool.Error == "" && strings.Contains(content, "fixture test")
		}
	}
	body, err := os.ReadFile(filepath.Join(ws, "math.go"))
	if err != nil {
		return err
	}
	fmt.Printf("math.go:\n%s", body)
	if handle.Status().Status != string(agent.EventCompleted) || !strings.Contains(string(body), "return a + b") || !verified || (!reviewed && os.Getenv("GRAPE_REAL_MODEL") != "1") {
		return fmt.Errorf("fixture verification incomplete (go test passed=%t, reviewer=%t)", verified, reviewed)
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
	entries, err := os.ReadDir(src)
	if err != nil {
		return "", err
	}
	dst, err := os.MkdirTemp("", "grape-coding-")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err == nil {
			err = os.WriteFile(filepath.Join(dst, entry.Name()), body, 0600)
		}
		if err != nil {
			os.RemoveAll(dst)
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
