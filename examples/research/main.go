// Command research compares two local benchmark reports without network access.
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
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

type report struct {
	Source string `json:"source"`
	P95    int    `json:"p95_ms"`
}

func reports(_ context.Context, in struct {
	Metric string `json:"metric"`
}) ([]report, error) {
	if in.Metric != "latency" {
		return nil, fmt.Errorf("unknown metric %q", in.Metric)
	}
	return []report{{"baseline", 120}, {"candidate", 90}}, nil
}

// The scripted model consumes the actual skill and tool results on each turn.
func analyze(_ context.Context, req *model.Request) (*model.Response, error) {
	for _, name := range []string{"run_command", "read_file", "git_status"} {
		if _, ok := req.Tools[name]; ok {
			return nil, fmt.Errorf("unexpected coding tool %s", name)
		}
	}
	var results []string
	var instructions bool
	for _, msg := range req.Messages {
		if strings.Contains(msg.Content, "cite both sources") {
			instructions = true
		}
		if msg.Role == model.RoleTool {
			results = append(results, msg.Content)
		}
	}
	switch len(results) {
	case 0:
		return mockmodel.ToolCall("skill_load", `{"skill":"compare"}`), nil
	case 1:
		if !instructions || !strings.Contains(results[0], "loaded: compare") {
			return nil, fmt.Errorf("missing comparison instructions")
		}
		return mockmodel.ToolCall("lookup_reports", `{"metric":"latency"}`), nil
	default:
		var rows []report
		if err := json.Unmarshal([]byte(results[1]), &rows); err != nil {
			return nil, err
		}
		if len(rows) != 2 || rows[0].P95 <= 0 {
			return nil, fmt.Errorf("invalid report pair")
		}
		a, b := rows[0], rows[1]
		text := fmt.Sprintf("%s %d ms, %s %d ms. P95 fell %.1f%%.", a.Source, a.P95, b.Source, b.P95, 100*float64(a.P95-b.P95)/float64(a.P95))
		return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage(text)}}}, nil
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "grape-research-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	skillRoot := filepath.Join(dir, "skills")
	path := filepath.Join(skillRoot, "compare")
	if err = os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: compare\ndescription: Compare latency reports\n---\nCompute the percentage change and cite both sources.\n"), 0600); err != nil {
		return err
	}
	cfg := config.Default()
	cfg.Workspace.Root = dir
	cfg.Workspace.Permission = agent.PermissionReadOnly
	cfg.Workspace.DisableCodingTools = true
	cfg.Skills.Paths = []string{skillRoot}
	cfg.Context.TotalMaxLLMCalls = config.Int(3)
	lookup := function.NewFunctionTool(reports, function.WithName("lookup_reports"), function.WithDescription("Return two fixture reports for the latency metric."))
	rt, err := trpcruntime.New(trpcruntime.Options{
		Config: &cfg, DataDir: dir,
		Instruction: "Compare the reports using the compare skill.",
		Model:       mockmodel.New("research", analyze),
		ToolSpecs:   []trpcruntime.ToolSpec{{Tool: lookup, Kind: "read"}},
	})
	if err != nil {
		return err
	}
	defer rt.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := rt.Start(ctx, agent.RunRequest{UserID: "analyst", SessionID: "latency", Message: "How did P95 latency change?"})
	if err != nil {
		return err
	}
	var answer agent.TextPayload
	for e := range h.Events() {
		fmt.Printf("%d %s %s\n", e.Sequence, e.Kind, e.Payload)
		if e.Kind == agent.EventText {
			if err := json.Unmarshal(e.Payload, &answer); err != nil {
				return err
			}
		}
	}
	if status := h.Status(); status.Status != string(agent.EventCompleted) || !strings.Contains(answer.Text, "25.0%") {
		return fmt.Errorf("research incomplete: %s %s", status.Status, answer.Text)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
