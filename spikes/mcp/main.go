package main

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	var _ mcp.Transport = &mcp.CommandTransport{}
	var _ mcp.Transport = &mcp.StreamableClientTransport{}
	fmt.Println("stdio client: mcp.CommandTransport; HTTP client: mcp.StreamableClientTransport")
}
