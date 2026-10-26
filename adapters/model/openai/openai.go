package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/panca/haythos/core/task"
)

const defaultBaseURL = "https://api.openai.com/v1"

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

type Adapter struct{ c Config }

func New(c Config) *Adapter {
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	return &Adapter{c: c}
}

type fn struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function fn     `json:"function"`
}

type message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function fn     `json:"function"`
}

type request struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []toolDef `json:"tools,omitempty"`
}

type response struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Function names only allow [a-zA-Z0-9_-]; kernel names use dots.
func wireName(n string) string  { return strings.ReplaceAll(n, ".", "__") }
func localName(n string) string { return strings.ReplaceAll(n, "__", ".") }

func str(s string) *string { return &s }

func (a *Adapter) Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	req := request{Model: a.c.Model}
	for _, m := range history {
		switch m.Role {
		case task.RoleSystem, task.RoleUser:
			req.Messages = append(req.Messages, message{Role: string(m.Role), Content: str(m.Text)})
		case task.RoleAssistant:
			am := message{Role: "assistant"}
			if m.Text != "" {
				am.Content = str(m.Text)
			}
			for _, c := range m.Calls {
				am.ToolCalls = append(am.ToolCalls, toolCall{ID: c.ID, Type: "function", Function: fn{Name: wireName(c.Name), Arguments: string(c.Args)}})
			}
			req.Messages = append(req.Messages, am)
		case task.RoleTool:
			for _, r := range m.Results {
				content := r.Content
				if r.IsError {
					content = "error: " + content
				}
				req.Messages = append(req.Messages, message{Role: "tool", ToolCallID: r.CallID, Content: str(content)})
			}
		}
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, toolDef{Type: "function", Function: fn{Name: wireName(t.Name), Description: t.Description, Parameters: t.InputSchema}})
	}

	body, err := json.Marshal(req)
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("encode request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.c.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("authorization", "Bearer "+a.c.APIKey)
	resp, err := a.c.HTTP.Do(httpReq)
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("openai request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return task.AssistantTurn{}, fmt.Errorf("openai status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		return task.AssistantTurn{}, fmt.Errorf("decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return task.AssistantTurn{}, fmt.Errorf("openai response has no choices")
	}
	msg := out.Choices[0].Message
	turn := task.AssistantTurn{Usage: task.Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}}
	if msg.Content != nil {
		turn.Text = *msg.Content
	}
	for _, c := range msg.ToolCalls {
		turn.Calls = append(turn.Calls, task.ToolCall{ID: c.ID, Name: localName(c.Function.Name), Args: json.RawMessage(c.Function.Arguments)})
	}
	return turn, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
