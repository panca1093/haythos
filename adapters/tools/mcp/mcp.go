package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

const protocolVersion = "2024-11-05"

type Config struct {
	Name    string
	Command string
	Args    []string
	// Env is added to PATH and HOME; the kernel's own environment (vault key) is never passed on.
	Env []string
	// Trusted servers produce results that do not taint a task.
	Trusted bool
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Client is one MCP stdio server. It implements app.Tools for the tools that server lists.
type Client struct {
	cfg     Config
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	writeMu sync.Mutex
	nextID  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan response
	closed  chan struct{}
	readErr error

	specs []task.ToolSpec
}

var _ app.Tools = (*Client)(nil)

// Start launches the server, performs the handshake and caches its tool list. ctx must outlive the client: cancelling it kills the server.
func Start(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Name == "" || cfg.Command == "" {
		return nil, errors.New("start mcp server: name and command are required")
	}
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...) //nolint:gosec // command comes from the owner's config
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}, cfg.Env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdin of %s: %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open stdout of %s: %w", cfg.Name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start mcp server %s: %w", cfg.Name, err)
	}
	c := &Client{cfg: cfg, cmd: cmd, stdin: stdin, pending: map[int64]chan response{}, closed: make(chan struct{})}
	go c.readLoop(stdout)

	if err := c.handshake(ctx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("handshake with %s: %w", cfg.Name, err)
	}
	return c, nil
}

func (c *Client) handshake(ctx context.Context) error {
	if _, err := c.request(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "haythosd", "version": "0"},
	}); err != nil {
		return err
	}
	if err := c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return err
	}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.request(ctx, "tools/list", params)
		if err != nil {
			return err
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations struct {
					ReadOnlyHint bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return fmt.Errorf("decode tools/list: %w", err)
		}
		for _, t := range page.Tools {
			schema := t.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			c.specs = append(c.specs, task.ToolSpec{
				Name: c.cfg.Name + "." + t.Name, Description: t.Description, InputSchema: schema,
				Write: !t.Annotations.ReadOnlyHint, Untrusted: !c.cfg.Trusted,
			})
		}
		if page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

func (c *Client) List(scope []string) []task.ToolSpec {
	var out []task.ToolSpec
	for _, s := range c.specs {
		for _, n := range scope {
			if s.Name == n {
				out = append(out, s)
			}
		}
	}
	return out
}

func (c *Client) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	name, ok := strings.CutPrefix(call.Name, c.cfg.Name+".")
	if !ok {
		return task.ToolResult{}, fmt.Errorf("tool %q does not belong to mcp server %s", call.Name, c.cfg.Name)
	}
	args := call.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	var re *rpcError
	if errors.As(err, &re) {
		return task.ToolResult{CallID: call.ID, Content: fmt.Sprintf("mcp error %d: %s", re.Code, re.Message), IsError: true, Untrusted: !c.cfg.Trusted}, nil
	}
	if err != nil {
		return task.ToolResult{}, fmt.Errorf("call %s: %w", call.Name, err)
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return task.ToolResult{}, fmt.Errorf("decode result of %s: %w", call.Name, err)
	}
	var parts []string
	for _, p := range res.Content {
		if p.Type == "text" {
			parts = append(parts, p.Text)
		} else {
			parts = append(parts, "[non-text content omitted]")
		}
	}
	return task.ToolResult{CallID: call.ID, Content: strings.Join(parts, "\n"), IsError: res.IsError, Untrusted: !c.cfg.Trusted}, nil
}

func (c *Client) Exists(context.Context, string, string, string) (bool, error) {
	return false, app.ErrCannotVerify
}

// Close stops the server.
func (c *Client) Close() error {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

func (c *Client) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write to %s: %w", c.cfg.Name, err)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		if r.Error != nil {
			return nil, r.Error
		}
		return r.Result, nil
	case <-c.closed:
		select {
		case r := <-ch:
			if r.Error != nil {
				return nil, r.Error
			}
			return r.Result, nil
		default:
		}
		return nil, fmt.Errorf("mcp server %s exited: %w", c.cfg.Name, c.readErr)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var msg struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID == nil {
			continue
		}
		if msg.Method != "" {
			_ = c.send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "error": map[string]any{"code": -32601, "message": "method not supported"}})
			continue
		}
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		c.mu.Unlock()
		if ch != nil {
			ch <- response{Result: msg.Result, Error: msg.Error}
		}
	}
	c.readErr = sc.Err()
	if c.readErr == nil {
		c.readErr = io.EOF
	}
	close(c.closed)
}
