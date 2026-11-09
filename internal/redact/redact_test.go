package redact

import "testing"

func TestTextRemovesEnvValues(t *testing.T) {
	secret := "sk-test-secret-value"
	got := Text("authorization="+secret+" tail", []string{secret, "", "ab"})
	if got != "authorization=*** tail" {
		t.Fatalf("%q", got)
	}
}

func TestTruncateDropsTheTail(t *testing.T) {
	got, truncated := Truncate("abcdefghij", 4)
	if !truncated || got != "abcd" {
		t.Fatalf("%q %v", got, truncated)
	}
	same, truncated := Truncate("abcd", 4)
	if truncated || same != "abcd" {
		t.Fatalf("%q %v", same, truncated)
	}
}
