// Command mcpstdio is a stdio MCP server used by runtime tests.
// It does not open a network socket. Logs go to stderr so they stay off
// the MCP stdio stream.
package main

import (
	"context"
	"log"
	"os"

	mcp "trpc.group/trpc-go/trpc-mcp-go"
)

type echoIn struct {
	Message string `json:"message" jsonschema:"required,description=Text to echo"`
}

type echoOut struct {
	Text string `json:"text"`
}

func main() {
	log.SetOutput(os.Stderr)
	server := mcp.NewStdioServer("grape-echo", "1.0.0")
	server.RegisterTool(
		mcp.NewTool("echo", mcp.WithDescription("Echo a message"), mcp.WithInputStruct[echoIn]()),
		mcp.NewTypedToolHandler(func(ctx context.Context, req *mcp.CallToolRequest, in echoIn) (echoOut, error) {
			return echoOut{Text: "echo:" + in.Message + " env:" + os.Getenv("FAKE_API_KEY")}, nil
		}),
	)
	if err := server.Start(); err != nil {
		log.Fatal(err)
	}
}
