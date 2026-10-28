package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

func TestMain(m *testing.M) {
	if os.Getenv("HAYTHOS_FAKE_MCP") == "1" {
		serveFake()
		return
	}
	os.Exit(m.Run())
}

func serveFake() {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Cursor    string         `json:"cursor"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		reply := func(result any) {
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		text := func(s string, isErr bool) {
			reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}, "isError": isErr})
		}
		switch req.Method {
		case "initialize":
			reply(map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake"}})
		case "tools/list":
			if req.Params.Cursor == "" {
				reply(map[string]any{
					"tools":      []any{map[string]any{"name": "read", "description": "r", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}},
					"nextCursor": "p2",
				})
			} else {
				reply(map[string]any{"tools": []any{map[string]any{"name": "write", "description": "w"}}})
			}
		case "tools/call":
			switch req.Params.Name {
			case "read":
				text("read:"+req.Params.Name, false)
			case "env":
				text("vault="+os.Getenv("HAYTHOS_VAULT_KEY"), false)
			case "fail":
				text("it broke", true)
			case "image":
				reply(map[string]any{"content": []any{map[string]any{"type": "image", "data": "AAAA"}, map[string]any{"type": "text", "text": "after"}}})
			case "slow":
				time.Sleep(5 * time.Second)
				text("late", false)
			default:
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
			}
		}
	}
}

func start(t *testing.T, trusted bool) *Client {
	t.Helper()
	t.Setenv("HAYTHOS_VAULT_KEY", "super-secret")
	c, err := Start(context.Background(), Config{Name: "srv", Command: os.Args[0], Env: []string{"HAYTHOS_FAKE_MCP=1"}, Trusted: trusted})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestListNamesTrustAndWriteFlag(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trusted   bool
		untrusted bool
	}{{"untrusted server", false, true}, {"trusted server", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := start(t, tc.trusted)
			specs := c.List([]string{"srv.read", "srv.write", "srv.nope"})
			if len(specs) != 2 {
				t.Fatalf("specs = %+v, want 2 (second page included)", specs)
			}
			byName := map[string]task.ToolSpec{}
			for _, s := range specs {
				byName[s.Name] = s
				if s.Untrusted != tc.untrusted {
					t.Errorf("%s Untrusted = %v, want %v", s.Name, s.Untrusted, tc.untrusted)
				}
			}
			if byName["srv.read"].Write {
				t.Error("readOnlyHint tool should not be a write")
			}
			if !byName["srv.write"].Write {
				t.Error("unannotated tool must be treated as a write")
			}
			if string(byName["srv.write"].InputSchema) != `{"type":"object"}` {
				t.Errorf("missing schema = %s, want empty object schema", byName["srv.write"].InputSchema)
			}
		})
	}
}

func TestCall(t *testing.T) {
	c := start(t, false)
	for _, tc := range []struct {
		name    string
		tool    string
		content string
		isErr   bool
		wantErr bool
	}{
		{"text", "srv.read", "read:read", false, false},
		{"server-reported failure", "srv.fail", "it broke", true, false},
		{"non-text part is omitted", "srv.image", "[non-text content omitted]\nafter", false, false},
		{"rpc error becomes a tool error", "srv.missing", "mcp error -32602: unknown tool", true, false},
		{"foreign tool is refused", "other.read", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Call(context.Background(), task.ToolCall{ID: "c1", Name: tc.tool, Args: json.RawMessage(`{}`)})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if res.Content != tc.content || res.IsError != tc.isErr || !res.Untrusted || res.CallID != "c1" {
				t.Fatalf("result = %+v, want content %q isError %v untrusted", res, tc.content, tc.isErr)
			}
		})
	}
}

func TestServerDoesNotInheritKernelEnvironment(t *testing.T) {
	c := start(t, true)
	res, err := c.Call(context.Background(), task.ToolCall{ID: "c1", Name: "srv.env"})
	if err != nil || res.Content != "vault=" {
		t.Fatalf("result = %+v err %v, want an empty vault key", res, err)
	}
}

func TestCallHonoursContext(t *testing.T) {
	c := start(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, task.ToolCall{ID: "c1", Name: "srv.slow"}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestExistsCannotVerify(t *testing.T) {
	c := start(t, true)
	if _, err := c.Exists(context.Background(), "srv.write", "", "k"); !errors.Is(err, app.ErrCannotVerify) {
		t.Fatalf("err = %v, want ErrCannotVerify", err)
	}
}

func TestServerExitFailsPendingCalls(t *testing.T) {
	c := start(t, true)
	_ = c.cmd.Process.Kill()
	<-c.closed
	if _, err := c.Call(context.Background(), task.ToolCall{ID: "c1", Name: "srv.read"}); err == nil {
		t.Fatal("expected an error after the server exited")
	}
}

func TestStartRejectsMissingCommand(t *testing.T) {
	if _, err := Start(context.Background(), Config{Name: "x", Command: "/nonexistent/binary"}); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := Start(context.Background(), Config{Name: "x"}); err == nil {
		t.Fatal("expected an error for empty command")
	}
}
