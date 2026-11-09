// Package journal writes an optional NDJSON event log.
//
// The journal is a debug export. It is not session state, memory, or a
// replayable event bus. Large fields are truncated and configured secrets
// are removed before the line is written.
package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/lookerjin/grape-core/agent"
	"github.com/lookerjin/grape-core/internal/redact"
)

// Writer appends redacted events.
type Writer struct {
	path    string
	max     int
	secrets []string
	mu      sync.Mutex
}

// Open creates or appends to path. max is the per-string truncation limit.
func Open(path string, max int, secrets []string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Writer{path: path, max: max, secrets: append([]string(nil), secrets...)}, nil
}

// Append writes one event as a single JSON line.
func (w *Writer) Append(evt agent.Event) error {
	if w == nil {
		return nil
	}
	evt.Payload = redactPayload(evt.Payload, w.max, w.secrets)
	raw, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	line := redact.Text(string(raw), w.secrets)
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append([]byte(line), '\n'))
	return err
}

func redactPayload(payload json.RawMessage, max int, secrets []string) json.RawMessage {
	if len(payload) == 0 {
		return payload
	}
	text := redact.Text(string(payload), secrets)
	short, truncated := redact.Truncate(text, max)
	if !truncated {
		return json.RawMessage(short)
	}
	var generic map[string]any
	if err := json.Unmarshal([]byte(short), &generic); err == nil {
		generic["truncated"] = true
		out, err := json.Marshal(generic)
		if err == nil {
			return out
		}
	}
	wrapper, _ := json.Marshal(map[string]any{
		"truncated": true,
		"preview":   short,
	})
	return wrapper
}
