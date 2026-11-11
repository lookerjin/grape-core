// Command recovery exercises the public SDK against an offline HTTP model
// fixture. All workspace writes and recovery operations target a temporary directory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/config"
	"github.com/lookerjin/grape-core/model"
	core "github.com/lookerjin/grape-core/runtime/trpc"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir, e := os.MkdirTemp("", "grape-recovery-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Messages []struct{ Role, Content string }
		}
		if e := json.NewDecoder(r.Body).Decode(&q); e != nil || len(q.Messages) == 0 {
			http.Error(w, "invalid request", 400)
			return
		}
		msg := map[string]any{"role": "assistant", "content": "remembered baseline"}
		stop := "stop"
		if last := q.Messages[len(q.Messages)-1]; last.Role == "user" && last.Content == "edit" {
			stop = "tool_calls"
			msg["tool_calls"] = []any{map[string]any{"id": "save", "type": "function", "function": map[string]any{"name": "save_file", "arguments": `{"file_name":"note.txt","contents":"agent edit","overwrite":true}`}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": stop}}})
	}))
	defer server.Close()
	reg, e := model.Open(model.Options{Database: filepath.Join(dir, "models.db"), Scope: model.Scope{App: "recovery", User: "local"}, Store: &model.MemoryStore{}, AllowLoopbackHTTP: true})
	if e != nil {
		return e
	}
	defer reg.Close()
	conn, e := reg.AddKey(ctx, model.Provider{ID: "fixture", Endpoint: server.URL, Protocol: model.Chat}, "offline", "fixture-key")
	if e != nil {
		return e
	}
	e = reg.SetDefault(ctx, model.Ref{ConnectionID: conn.ID, ModelID: "local"})
	if e != nil {
		return e
	}
	ws := filepath.Join(dir, "workspace")
	if e = os.Mkdir(ws, 0700); e != nil {
		return e
	}
	path := filepath.Join(ws, "note.txt")
	if e = os.WriteFile(path, []byte("baseline"), 0600); e != nil {
		return e
	}
	cfg := config.Default()
	cfg.Workspace.Root = ws
	cfg.Workspace.Permission = agent.PermissionWorkspaceWrite
	stream := false
	cfg.Model.Generation.Stream = &stream
	rt, e := core.New(core.Options{AppName: "recovery", ModelRegistry: reg, Config: &cfg, DataDir: dir, TurnCheckpoints: true})
	if e != nil {
		return e
	}
	defer rt.Close()
	turn := func(msg string) (string, error) {
		h, e := rt.Start(ctx, agent.RunRequest{UserID: "local", SessionID: "main", Message: msg})
		if e != nil {
			return "", e
		}
		for range h.Events() {
		}
		if h.Status().Status != "completed" {
			return "", fmt.Errorf("run: %+v", h.Status())
		}
		return h.ID(), nil
	}
	base, e := turn("baseline")
	if e != nil {
		return e
	}
	if _, e = turn("edit"); e != nil {
		return e
	}
	fork, e := rt.ForkSession(ctx, "local", "main", base)
	if e != nil {
		return e
	}
	a, e := rt.AskSide(ctx, "local", fork, "what was the first turn?", nil)
	if e != nil {
		return e
	}
	fmt.Println("fork side answer:", a.Text)
	p, e := rt.PreviewFiles(ctx, "local", "main", base)
	if e != nil {
		return e
	}
	if len(p.Files) != 1 || p.Files[0].Conflict != "" || p.Files[0].Path != "note.txt" {
		return errors.New("unexpected restore preview")
	}
	if e = os.WriteFile(path, []byte("editor change"), 0600); e != nil {
		return e
	}
	result, e := rt.RestoreFiles(ctx, "local", "main", base, p.Digest)
	if e == nil || result.Status != "refused" {
		return errors.New("stale preview accepted")
	}
	fmt.Println("stale preview:", result.Status)
	if e = os.WriteFile(path, []byte("agent edit"), 0600); e != nil {
		return e
	}
	p, e = rt.PreviewFiles(ctx, "local", "main", base)
	if e != nil {
		return e
	}
	// The demo owns this temporary workspace. An interactive product confirms this exact preview with its user.
	result, e = rt.RestoreFiles(ctx, "local", "main", base, p.Digest)
	if e != nil {
		return e
	}
	backup, e := rt.RecoveryBackup(ctx, "local", "main", result.ID, ws, "note.txt")
	if e != nil {
		return e
	}
	if e = rt.RewindSession(ctx, "local", "main", base); e != nil {
		return e
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	if string(raw) != "baseline" || string(backup.Data) != "agent edit" {
		return errors.New("file or backup mismatch")
	}
	fmt.Printf("workspace=%q backup=%q\n", raw, backup.Data)
	log, e := rt.Recoveries(ctx, "local", "main")
	for _, r := range log {
		fmt.Println(r.Kind, r.Status)
	}
	return e
}
