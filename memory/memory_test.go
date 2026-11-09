package memory

import (
	"os"
	"strings"
	"testing"
)

func TestPutRecallAndIndex(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("practices/tests.md", "Run the focused package first."); err != nil {
		t.Fatal(err)
	}
	if err := store.Put("../escape.md", "no"); err == nil {
		t.Fatal("expected escape rejection")
	}
	hits, err := store.Recall("focused")
	if err != nil || len(hits) != 1 || hits[0].Path != "practices/tests.md" {
		t.Fatalf("%#v %v", hits, err)
	}
	index, err := os.ReadFile(store.Root() + "/MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "practices/tests.md") {
		t.Fatalf("index %s", index)
	}
	if err := os.Remove(store.Root() + "/MEMORY.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(store.Root()); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(store.Root() + "/MEMORY.md")
	if err != nil || !strings.Contains(string(again), "practices/tests.md") {
		t.Fatalf("rebuilt %s %v", again, err)
	}
}
