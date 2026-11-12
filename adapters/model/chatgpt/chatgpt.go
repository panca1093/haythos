package chatgpt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

const namespace = "alfred"

var ErrUsageLimit = errors.New("ChatGPT plan usage limit reached, see https://chatgpt.com/settings/usage (no API-key fallback)")

type Config struct {
	Model   string
	BaseURL string
	HTTP    *http.Client
	Session *Session
}

// Adapter runs one Step on the public Responses API with the user's ChatGPT plan token.
type Adapter struct {
	c     Config
	mu    sync.Mutex
	model string
}

func New(c Config) *Adapter {
	if c.BaseURL == "" {
		c.BaseURL = resource
	}
	if c.HTTP == nil {
		c.HTTP = http.DefaultClient
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return &Adapter{c: c, model: c.Model}
}

type ModelInfo struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
}

type inputItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    any    `json:"output,omitempty"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Tools       []tool          `json:"tools,omitempty"`
}

// request carries only fields the plan route accepts; temperature, max_output_tokens, previous_response_id etc. are rejected there.
type request struct {
	Model        string      `json:"model"`
	Instructions string      `json:"instructions,omitempty"`
	Input        []inputItem `json:"input"`
	Tools        []tool      `json:"tools,omitempty"`
	CacheKey     string      `json:"prompt_cache_key,omitempty"`
	Store        bool        `json:"store"`
	Stream       bool        `json:"stream"`
}

type outputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type event struct {
	Type     string     `json:"type"`
	Item     outputItem `json:"item"`
	Code     string     `json:"code"`
	Message  string     `json:"message"`
	Error    *apiError  `json:"error"`
	Response struct {
		Status string       `json:"status"`
		Output []outputItem `json:"output"`
		Error  *apiError    `json:"error"`
		Usage  struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		Incomplete struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

var wireNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// wireName maps a kernel name (dots allowed) to the API's [a-zA-Z0-9_-]{1,64} function names.
func wireName(n string) (string, error) {
	w := strings.ReplaceAll(n, ".", "__")
	if !wireNameRE.MatchString(w) {
		return "", fmt.Errorf("tool name %q cannot be sent to chatgpt (allowed: letters, digits, '_', '-', '.'; at most 64 characters after encoding)", n)
	}
	return w, nil
}

func (a *Adapter) Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	token, err := a.c.Session.AccessToken(ctx)
	if err != nil {
		return task.AssistantTurn{}, err
	}
	model, err := a.modelName(ctx, token)
	if err != nil {
		return task.AssistantTurn{}, err
	}
	built, names, err := buildRequest(model, history, tools)
	if err != nil {
		return task.AssistantTurn{}, err
	}
	built.CacheKey = app.CacheKeyFrom(ctx)
	body, err := json.Marshal(built)
	if err != nil {
		return task.AssistantTurn{}, fmt.Errorf("encode request: %w", err)
	}
	resp, err := a.do(ctx, http.MethodPost, "/responses", body, token)
	if err != nil {
		return task.AssistantTurn{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	turn, err := parseStream(resp.Body, token, names)
	if err != nil && ctx.Err() != nil {
		return task.AssistantTurn{}, fmt.Errorf("chatgpt stream: %w", ctx.Err())
	}
	return turn, err
}

type outputPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

func toolOutput(out string, images []task.Image) any {
	if len(images) == 0 {
		return out
	}
	parts := []outputPart{{Type: "input_text", Text: out}}
	for _, img := range images {
		parts = append(parts, outputPart{Type: "input_image", ImageURL: "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)})
	}
	return parts
}

// buildRequest also returns the wire-name to kernel-name map used to resolve the model's tool calls.
func buildRequest(model string, history []task.Message, tools []task.ToolSpec) (request, map[string]string, error) {
	req := request{Model: model, Input: []inputItem{}, Store: false, Stream: true}
	var system []string
	for _, m := range history {
		switch m.Role {
		case task.RoleSystem:
			if m.Context {
				req.Input = append(req.Input, inputItem{Role: "user", Content: m.Text})
			} else {
				system = append(system, m.Text)
			}
		case task.RoleUser:
			req.Input = append(req.Input, inputItem{Role: "user", Content: m.Text})
		case task.RoleAssistant:
			if m.Text != "" {
				req.Input = append(req.Input, inputItem{Role: "assistant", Content: m.Text})
			}
			for _, c := range m.Calls {
				args := string(c.Args)
				if args == "" {
					args = "{}"
				}
				w, err := wireName(c.Name)
				if err != nil {
					return request{}, nil, err
				}
				req.Input = append(req.Input, inputItem{Type: "function_call", CallID: c.ID, Namespace: namespace, Name: w, Arguments: args})
			}
		case task.RoleTool:
			for _, r := range m.Results {
				out := r.Content
				if r.IsError {
					out = "error: " + out
				}
				req.Input = append(req.Input, inputItem{Type: "function_call_output", CallID: r.CallID, Output: toolOutput(out, r.Images)})
			}
		}
	}
	req.Instructions = strings.Join(system, "\n\n")
	tools = slices.Clone(tools)
	slices.SortFunc(tools, func(a, b task.ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	names := make(map[string]string, len(tools))
	if len(tools) > 0 {
		ns := tool{Type: "namespace", Name: namespace, Description: "Alfred kernel tools. Every call is checked by policy and may need the user's approval."}
		for _, t := range tools {
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			w, err := wireName(t.Name)
			if err != nil {
				return request{}, nil, err
			}
			if prev, dup := names[w]; dup {
				return request{}, nil, fmt.Errorf("tool names %q and %q collide as %q on the chatgpt wire", prev, t.Name, w)
			}
			names[w] = t.Name
			ns.Tools = append(ns.Tools, tool{Type: "function", Name: w, Description: t.Description, Parameters: params})
		}
		req.Tools = []tool{ns}
	}
	return req, names, nil
}

func parseStream(body io.Reader, token string, names map[string]string) (task.AssistantTurn, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var items []outputItem
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		data = strings.TrimSpace(data)
		if !ok || data == "" || data == "[DONE]" {
			continue
		}
		var e event
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return task.AssistantTurn{}, fmt.Errorf("decode stream event: %w", err)
		}
		switch e.Type {
		case "response.output_item.done":
			items = append(items, e.Item)
		case "response.completed":
			if len(items) == 0 {
				items = e.Response.Output
			}
			turn, err := toTurn(items, names)
			turn.Usage = task.Usage{InputTokens: e.Response.Usage.InputTokens, OutputTokens: e.Response.Usage.OutputTokens, CacheReadTokens: e.Response.Usage.InputTokensDetails.CachedTokens}
			return turn, err
		case "response.failed":
			ae := apiError{Code: "unknown_error"}
			if e.Response.Error != nil {
				ae = *e.Response.Error
			}
			return task.AssistantTurn{}, planError(0, ae, token)
		case "response.incomplete":
			return task.AssistantTurn{}, fmt.Errorf("chatgpt response incomplete (%s)", e.Response.Incomplete.Reason)
		case "error":
			ae := apiError{Code: e.Code, Message: e.Message}
			if e.Error != nil {
				ae = *e.Error
			}
			return task.AssistantTurn{}, planError(0, ae, token)
		}
	}
	if err := sc.Err(); err != nil {
		return task.AssistantTurn{}, fmt.Errorf("read stream: %w", err)
	}
	return task.AssistantTurn{}, errors.New("chatgpt stream ended without response.completed")
}

func toTurn(items []outputItem, names map[string]string) (task.AssistantTurn, error) {
	var turn task.AssistantTurn
	for _, it := range items {
		switch it.Type {
		case "message":
			for _, c := range it.Content {
				if c.Type == "output_text" {
					turn.Text += c.Text
				}
			}
		case "function_call":
			// TODO: uncertain — docs do not show whether output function_call items always carry namespace; "" is accepted, verify live
			if it.Namespace != "" && it.Namespace != namespace {
				return task.AssistantTurn{}, fmt.Errorf("chatgpt called a tool in unexpected namespace %q", it.Namespace)
			}
			if it.CallID == "" || it.Name == "" || !json.Valid([]byte(it.Arguments)) {
				return task.AssistantTurn{}, errors.New("chatgpt returned an invalid tool call")
			}
			name, known := names[it.Name]
			if !known {
				return task.AssistantTurn{}, fmt.Errorf("chatgpt called unknown tool %q", it.Name)
			}
			turn.Calls = append(turn.Calls, task.ToolCall{ID: it.CallID, Name: name, Args: json.RawMessage(it.Arguments)})
		}
	}
	return turn, nil
}

// planError never falls back to API-key billing and never retries; status is 0 for errors that arrive inside the stream.
// The server message is kept for diagnosis with the access token scrubbed out.
func planError(status int, e apiError, token string) error {
	msg := strings.ReplaceAll(e.Message, token, "[redacted]")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if e.Code == "subscription_sharing_usage_limit_exceeded" {
		return fmt.Errorf("%w: %s", ErrUsageLimit, msg)
	}
	if status != 0 {
		return fmt.Errorf("chatgpt HTTP %d %s: %s (no retry, no API-key fallback)", status, e.Code, msg)
	}
	return fmt.Errorf("chatgpt response failed %s: %s (no retry, no API-key fallback)", e.Code, msg)
}

func (a *Adapter) do(ctx context.Context, method, path string, body []byte, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
	}
	if key := app.CacheKeyFrom(ctx); key != "" {
		// ChatGPT takes cache affinity from this header, as Codex sends it; the body key alone gave no hits.
		req.Header.Set("session-id", key)
	}
	resp, err := a.c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("chatgpt %s: %w", path, ctx.Err())
		}
		return nil, fmt.Errorf("chatgpt %s: %w", path, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var e struct {
		Error  *apiError `json:"error"`
		Detail string    `json:"detail"`
	}
	_ = json.Unmarshal(raw, &e)
	ae := apiError{Message: e.Detail}
	if e.Error != nil {
		ae = *e.Error
	}
	return nil, planError(resp.StatusCode, ae, token)
}

// Models lists the account's displayable models in server order.
func (a *Adapter) Models(ctx context.Context) ([]ModelInfo, error) {
	token, err := a.c.Session.AccessToken(ctx)
	if err != nil {
		return nil, err
	}
	return a.models(ctx, token)
}

func (a *Adapter) List(ctx context.Context) ([]app.ModelChoice, error) {
	list, err := a.Models(ctx)
	if err != nil {
		return nil, err
	}
	choices := make([]app.ModelChoice, 0, len(list))
	for _, m := range list {
		choices = append(choices, app.ModelChoice{Value: m.Slug, Name: m.DisplayName})
	}
	return choices, nil
}

func (a *Adapter) WithModel(name string) app.Model {
	cfg := a.c
	cfg.Model = name
	return New(cfg)
}

func (a *Adapter) ContextWindow() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.model == "gpt-6.1-sol" {
		return 1_000_000
	}
	return 0
}

func (a *Adapter) models(ctx context.Context, token string) ([]ModelInfo, error) {
	resp, err := a.do(ctx, http.MethodGet, "/models", nil, token)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	var list []ModelInfo
	for _, m := range out.Models {
		if m.Visibility == "list" && m.Slug != "" {
			list = append(list, m)
		}
	}
	return list, nil
}

// modelName is the configured model, else the first listed model of the account (looked up once).
func (a *Adapter) modelName(ctx context.Context, token string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.model != "" {
		return a.model, nil
	}
	list, err := a.models(ctx, token)
	if err != nil {
		return "", fmt.Errorf("pick default chatgpt model: %w", err)
	}
	if len(list) == 0 {
		return "", errors.New("this ChatGPT account lists no models available for plan usage")
	}
	a.model = list[0].Slug
	return a.model, nil
}
