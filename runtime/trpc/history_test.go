package trpcruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
	"trpc.group/trpc-go/trpc-agent-go/tool/function"
)

func TestPromptProjectionKeepsPersistedHistory(t *testing.T) {
	cfg := quietConfig(t)
	cfg.Context.MaxHistoryMessages = 2
	cfg.Context.Compaction = true
	cfg.Context.OversizedToolResultTokens = 128
	cfg.Model.APIKeyEnv = "GRAPE_HISTORY_KEY"
	t.Setenv(cfg.Model.APIKeyEnv, "private-fixture-key")
	raw := strings.Repeat(" evidence ", 3000)
	lookup := function.NewFunctionTool(func(context.Context, struct{}) (string, error) { return raw, nil }, function.WithName("lookup"))
	mdl := mockmodel.New("history", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if lastUserText(req) == "earlier private-fixture-key" {
			return mockmodel.Echo(ctx, req)
		}
		for _, msg := range req.Messages {
			if msg.Role == model.RoleTool {
				return mockmodel.Echo(ctx, req)
			}
		}
		return mockmodel.ToolCall("lookup", "{}"), nil
	})
	dir := t.TempDir()
	opts := Options{Config: &cfg, DataDir: dir, Model: mdl, ToolSpecs: []ToolSpec{{Tool: lookup, Kind: "read"}}}
	rt, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	for _, msg := range []string{"earlier private-fixture-key", "lookup evidence"} {
		h, err := rt.Start(context.Background(), agent.RunRequest{UserID: "u", SessionID: "s", Message: msg})
		if err != nil {
			t.Fatal(err)
		}
		ev := collect(t, h)
		if !ev[len(ev)-1].Terminal() || h.Status().Status != "completed" {
			t.Fatal(h.Status())
		}
	}
	requests := mdl.Requests()
	last := requests[len(requests)-1]
	var compact string
	for _, msg := range last.Messages {
		if msg.Role == model.RoleTool {
			compact = msg.Content
		}
		if strings.Contains(msg.Content, "private-fixture-key") {
			t.Fatal("old turn survived window")
		}
	}
	if compact == "" || len(compact) >= len(raw) {
		t.Fatalf("projected tool content size %d, original %d", len(compact), len(raw))
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.SessionHistory(context.Background(), "u", "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	var full, call bool
	for _, row := range rows {
		if row.Role == "tool" && strings.Contains(row.Text, raw) {
			full = true
		}
		if len(row.ToolCalls) > 0 && row.ToolCalls[0].Name == "lookup" {
			call = true
		}
	}
	if len(rows) == 0 {
		t.Fatal("empty history after restart")
	}
	if rows[0].Text != "earlier ***" || !full || !call {
		t.Fatalf("rows=%d first=%q tool=%t call=%t", len(rows), rows[0].Text, full, call)
	}
	if _, err := reopened.SessionHistory(context.Background(), "another", "s", 0); !errors.Is(err, agent.ErrSession) {
		t.Fatalf("cross-user read: %v", err)
	}
}
