package mcp

import (
	"context"
	"errors"
	"testing"
)

func TestClaudeHeaderRefParses(t *testing.T) {
	raw := []byte(`{"mcpServers":{"demo":{"url":"http://127.0.0.1/mcp","headers":{"Authorization":"${TOKEN}"}}}}`)
	servers, err := ParseClaudeConfig(raw, "plugin")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Headers["Authorization"] != "${TOKEN}" || servers[0].Enabled {
		t.Fatalf("%#v", servers)
	}
}

func TestMergeOrderAndRedaction(t *testing.T) {
	user := []Server{{Name: "a", Transport: "stdio", Command: "echo", Enabled: true, Headers: map[string]string{"Authorization": "env:TOKEN"}}}
	project := []Server{{Name: "a", Transport: "stdio", Command: "other", Enabled: false}, {Name: "b", Transport: "sse", URL: "http://127.0.0.1/mcp", Enabled: true}}
	got, err := Merge(user, project)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[0].Command != "other" || got[0].Enabled || got[1].Name != "b" {
		t.Fatalf("%#v", got)
	}
	if _, err := Merge([]Server{{Name: "x", Headers: map[string]string{"Authorization": "sk-plaintext"}}}); err == nil {
		t.Fatal("expected header rejection")
	}
}

func TestStdioPermissionDoesNotConnect(t *testing.T) {
	called := false
	server := Server{Name: "demo", Transport: "stdio", Command: "echo", Enabled: true}
	_, err := Connect(context.Background(), server, func(command string) error {
		return errors.New("denied")
	}, ConnectorFunc(func(ctx context.Context, server Server) (Status, error) {
		called = true
		return Status{}, nil
	}))
	if err == nil {
		t.Fatal("expected denial")
	}
	if called {
		t.Fatal("connector ran after denial")
	}
}

func TestParseClaudeConfig(t *testing.T) {
	raw := []byte(`{"mcpServers":{"z":{"command":"echo","args":["z"]},"a":{"url":"http://127.0.0.1/sse"}}}`)
	got, err := ParseClaudeConfig(raw, "plugin")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "a" || got[0].Transport != "sse" || got[1].Name != "z" || got[1].Transport != "stdio" {
		t.Fatalf("%#v", got)
	}
}

type ConnectorFunc func(context.Context, Server) (Status, error)

func (f ConnectorFunc) Connect(ctx context.Context, server Server) (Status, error) {
	return f(ctx, server)
}
