package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestTerminalAndJSONRoundTrip(t *testing.T) {
	evt := Event{
		RunID:     "run",
		SessionID: "sess",
		AgentID:   "grape",
		Kind:      EventCompleted,
		Sequence:  4,
		Time:      time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC),
		Payload:   MustPayload(TerminalPayload{Status: "completed"}),
		Usage:     &Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
	}
	if !evt.Terminal() {
		t.Fatal("completed is not terminal")
	}
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.RunID != evt.RunID || back.SessionID != evt.SessionID || back.Sequence != 4 || !back.Terminal() {
		t.Fatalf("%#v", back)
	}
	if back.Usage == nil || back.Usage.TotalTokens != 5 {
		t.Fatalf("usage %#v", back.Usage)
	}
	var body TerminalPayload
	if err := json.Unmarshal(back.Payload, &body); err != nil || body.Status != "completed" {
		t.Fatalf("payload %s err %v", back.Payload, err)
	}
	text := Event{Kind: EventText, Payload: MustPayload(TextPayload{Text: "hi"})}
	if text.Terminal() {
		t.Fatal("text is terminal")
	}
	failed := Event{Kind: EventFailed}
	cancelled := Event{Kind: EventCancelled}
	if !failed.Terminal() || !cancelled.Terminal() {
		t.Fatal("failure states are not terminal")
	}
}

func TestErrorUnwrap(t *testing.T) {
	err := Wrap(ErrPermissionDenied, "write", errors.New("no"))
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatal(err)
	}
	var pub *Error
	if !errors.As(err, &pub) || pub.Op != "write" {
		t.Fatalf("%#v", err)
	}
	if !AsKind(Invalid("model", errors.New("missing")), ErrInvalidConfig) {
		t.Fatal("invalid config kind")
	}
}
