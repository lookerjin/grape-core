package trpcruntime

import (
	"bytes"
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
	if err := os.WriteFile(filepath.Join(ws, "keep", "0big.txt"), []byte(big), 0o644); err != nil {
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
		{"read_file", `{"file_name":"keep/0big.txt"}`},
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
	if strings.Contains(blob, `"skipped":0`) {
		t.Fatalf("search did not count the oversized file: %s", blob)
	}
}

func TestRangedReadAllowsLongLinesAndLargeFiles(t *testing.T) {
	ws := t.TempDir()
	long := strings.Repeat("L", 80<<10)
	if err := os.WriteFile(filepath.Join(ws, "long.txt"), []byte(long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var big strings.Builder
	for i := 0; i < 200; i++ {
		big.WriteString(strings.Repeat("a", 40))
		big.WriteByte('\n')
	}
	big.WriteString("TAIL-LINE\n")
	if err := os.WriteFile(filepath.Join(ws, "wide.txt"), []byte(big.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	longBlob := runRead(t, ws, 1<<20, `{"file_name":"long.txt","offset":1,"limit":1}`)
	if strings.Contains(longBlob, "token too long") || strings.Contains(longBlob, "exceeds") {
		t.Fatalf("long line refused: %s", longBlob)
	}
	if !strings.Contains(longBlob, long[:32]) {
		t.Fatal("long line missing from read_file")
	}
	huge := filepath.Join(ws, "huge.txt")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte{'H'}, 2<<20)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("\nTAIL-AFTER-HUGE\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	skipBlob := runRead(t, ws, 1<<20, `{"file_name":"huge.txt","offset":2,"limit":1}`)
	if strings.Contains(skipBlob, "exceeds") {
		t.Fatalf("offset could not skip a long line: %s", skipBlob)
	}
	if !strings.Contains(skipBlob, "TAIL-AFTER-HUGE") {
		t.Fatalf("line after a long line missing: %s", skipBlob)
	}
	tailBlob := runRead(t, ws, 4096, `{"file_name":"wide.txt","offset":201,"limit":1}`)
	if strings.Contains(tailBlob, "exceeds") {
		t.Fatalf("late line refused because the file is larger than the cap: %s", tailBlob)
	}
	if !strings.Contains(tailBlob, "TAIL-LINE") {
		t.Fatalf("late line missing: %s", tailBlob)
	}
}

func runRead(t *testing.T, ws string, max int, args string) string {
	t.Helper()
	cfg := config.Default()
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	cfg.Workspace.Root = ws
	cfg.Workspace.MaxReadBytes = max
	cfg.Journal.Enabled = false
	cfg.Model.Generation.Stream = boolPtr(false)
	var step int
	mdl := mockmodel.New("range", func(ctx context.Context, req *model.Request) (*model.Response, error) {
		if step > 0 || hasToolMessage(req) {
			return &model.Response{Choices: []model.Choice{{Message: model.NewAssistantMessage("done")}}}, nil
		}
		step++
		return toolCall("read_file", args), nil
	})
	rt, err := New(Options{Config: &cfg, Model: mdl, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	h, err := rt.Start(context.Background(), agent.RunRequest{
		UserID: "u", SessionID: "range", Workspace: ws, Message: "read",
	})
	if err != nil {
		t.Fatal(err)
	}
	return eventBlob(collect(t, h))
}
