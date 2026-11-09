package trpcruntime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/internal/mockmodel"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestReadAndSearchStayInsideLimits(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "line-one\nline-two\nNEEDLE-keep\nline-four\n"
	if err := os.WriteFile(filepath.Join(ws, "keep", "note.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "keep", "a.txt"), []byte("NEEDLE-keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "other", "note.txt"), []byte("NEEDLE-other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("x", 80)
	if err := os.WriteFile(filepath.Join(ws, "keep", "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Workspace.MaxReadBytes = 32
	cfg.Workspace.MaxSearchHits = 1
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	script := [][2]string{
		{"read_file", `{"file_name":"keep/note.txt","offset":2,"limit":1}`},
		{"read_file", `{"file_name":"keep/big.txt"}`},
		{"search_content", `{"path":"keep","content_pattern":"NEEDLE"}`},
	}
	var step int
	mdl := mockmodel.New("read", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if hasToolMessage(req) {
			if step >= len(script) {
				return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
			}
		}
		if step >= len(script) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		call := script[step]
		step++
		return toolCall(call[0], call[1]), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "read", Workspace: ws, Message: "look",
	})
	if err != nil {
		t.Fatal(err)
	}
	blob := eventBlob(collect(t, h))
	if !strings.Contains(blob, "line-two") {
		t.Fatalf("line window missing: %s", blob)
	}
	if strings.Contains(blob, "line-one") || strings.Contains(blob, "line-four") {
		t.Fatalf("read_file returned lines outside the window: %s", blob)
	}
	if !strings.Contains(blob, "exceeds") {
		t.Fatalf("oversized read was not refused: %s", blob)
	}
	if strings.Contains(blob, big) {
		t.Fatal("oversized file body was returned")
	}
	if strings.Contains(blob, "NEEDLE-other") {
		t.Fatalf("search left the requested directory: %s", blob)
	}
	if !strings.Contains(blob, "NEEDLE-keep") {
		t.Fatalf("search missed the path: %s", blob)
	}
}
