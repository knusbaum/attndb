//go:build ignore

// mcpclient is a throwaway MCP client for exercising the attndb serve daemon
// over Streamable HTTP. Run from the repo root:
//
//	go run scripts/mcpclient.go <endpoint> <query> [k]
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mcpclient <endpoint> <query> [k]")
		os.Exit(2)
	}
	endpoint, query := os.Args[1], os.Args[2]
	k := 5
	if len(os.Args) > 3 {
		k, _ = strconv.Atoi(os.Args[3])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer sess.Close()

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search_vault",
		Arguments: map[string]any{"query": query, "k": k},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	// StructuredContent is the typed searchOutput; print it as compact JSON.
	b, _ := json.Marshal(res.StructuredContent)
	fmt.Println(string(b))
}
