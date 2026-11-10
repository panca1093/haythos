package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/panca/haythos/core/task"
)

const defaultBaseURL = "https://api.anthropic.com"

type Config struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	HTTP      *http.Client
}

type Adapter struct{ c Config }

func New(c Config) *Adapter {
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 4096
	}
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	return &Adapter{c: c}
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    []block   `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []toolDef `json:"tools,omitempty"`
}

type response struct {
	Content []block `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// The API only accepts [a-zA-Z0-9_-] in tool names; kernel names use dots.
func wireName(n string) string  { return strings.ReplaceAll(n, ".", "__") }
func localName(n string) string { return strings.ReplaceAll(n, "__", ".") }

func (a *Adapter) Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	req := request{Model: a.c.Model, MaxTokens: a.c.MaxTokens}
	var system, summaries []string
	for _, m := range history {
		switch m.Role {
		case task.RoleSystem:
			if m.Context {
				summaries = append(summaries, "[earlier conversation summary] "+m.Text)
			} else {
				system = append(system, m.Text)
			}
		case task.RoleUser:
			req.Messages = append(req.Messages, message{Role: "user", Content: []block{{Type: "text", Text: m.Text}}})
		case task.RoleAssistant:
			var bs []block
			if m.Text != "" {
				bs = append(bs, block{Type: "text", Text: m.Text})
			}
			for _, c := range m.Calls {
				bs = append(bs, block{Type: "tool_use", ID: c.ID, Name: wireName(c.Name), Input: c.Args})
			}
			req.Messages = append(req.Messages, message{Role: "assistant", Content: bs})
		case task.RoleTool:
			var bs []block
			for _, r := range m.Results {
				bs = append(bs, block{Type: "tool_result", ToolUseID: r.CallID, Content: r.Content, IsError: r.IsError})
			}
			if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" && req.Messages[n-1].Content[0].Type == "tool_result" {
				req.Messages[n-1].Content = append(req.Messages[n-1].Content, bs...)
			} else {
				req.Messages = append(req.Messages, message{Role: "user", Content: bs})
			}
		}
	}
	if len(summaries) > 0 {
		head := message{Role: "user", Content: []block{{Type: "text", Text: strings.Join(summaries, "\n\n")}}}
		req.Messages = append([]message{head}, req.Messages...)
	}
	if len(system) > 0 {
		req.System = []block{{Type: "text", Text: strings.Join(system, "\n\n"), CacheControl: &cacheControl{Type: "ephemeral"}}}
	}
	if n := len(req.Messages); n > 0 {
		c := req.Messages[n-1].Content
		if len(c) > 0 {
			c[len(c)-1].CacheControl = &cacheControl{Type: "ephemeral"}
		}
	}
	sorted := slices.Clone(tools)
	slices.SortFunc(sorted, func(a, b task.ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	for _, t := range sorted {
		req.Tools = append(req.Tools, toolDef{Name: wireName(t.Name), Description: t.Description, InputSchema: t.InputSchema})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.c.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", a.c.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.c.HTTP.Do(httpReq)
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("anthropic request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return task.AssistantTurn{}, fmt.Errorf("anthropic status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return task.AssistantTurn{}, fmt.Errorf("decode response: %w", err)
	}
	turn := task.AssistantTurn{Usage: task.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}}
	for _, b := range out.Content {
		switch b.Type {
		case "text":
			turn.Text += b.Text
		case "tool_use":
			turn.Calls = append(turn.Calls, task.ToolCall{ID: b.ID, Name: localName(b.Name), Args: b.Input})
		}
	}
	return turn, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
