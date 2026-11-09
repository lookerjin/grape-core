package journal

import (
	"os"
	"strings"
	"testing"

	"github.com/lookerjin/grape-core/agent"
)

func TestAppendRedactsAndTruncatesToolOutput(t *testing.T) {
	path := t.TempDir() + "/journal.ndjson"
	secret := "sk-journal-secret"
	w, err := Open(path, 48, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("x", 400) + secret + strings.Repeat("y", 400)
	err = w.Append(agent.Event{
		RunID:   "run",
		Kind:    agent.EventToolEnd,
		Payload: agent.MustPayload(agent.ToolPayload{Name: "run_command", Result: agent.MustPayload(huge)}),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("secret leaked: %s", raw)
	}
	if len(raw) >= len(huge) {
		t.Fatalf("journal copied the full tool output: %d >= %d", len(raw), len(huge))
	}
	if !strings.Contains(string(raw), "truncated") {
		t.Fatalf("missing truncated marker: %s", raw)
	}
}
