package acceptance_test

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("HAYTHOS_FAKE_MCP") == "1" {
		fakeMCPServer()
		return
	}
	os.Exit(m.Run())
}

// fakeMCPServer speaks newline-delimited JSON-RPC 2.0 with one tool, "lookup".
func fakeMCPServer() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake", "version": "0"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": "look something up", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "lookup result"}}}
		default:
			result = map[string]any{}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}
