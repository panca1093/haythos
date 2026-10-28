// Package gbrain implements app.Brain over gbrain's MCP "verbs" surface (recall, remember, forget).
package gbrain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

// ServerName is the mcp.servers name the kernel expects for gbrain; its tools are gbrain.recall and so on.
const ServerName = "gbrain"

const (
	maxProvenance    = 500
	unverifiedMarker = "unverified"
	pageSource       = "import/mybook"
)

type Caller interface {
	Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error)
}

type Brain struct{ c Caller }

var _ app.Brain = (*Brain)(nil)

func New(c Caller) *Brain { return &Brain{c: c} }

// Provenance is stored in gbrain's free-text `provenance` field as "<source>" or "<source> unverified".
func encodeProvenance(src string, unverified bool) string {
	src = strings.Join(strings.Fields(src), "_")
	if unverified {
		src = truncate(src, maxProvenance-len(unverifiedMarker)-1) + " " + unverifiedMarker
	}
	return truncate(src, maxProvenance)
}

func decodeProvenance(p string) (src string, unverified bool) {
	f := strings.Fields(p)
	if len(f) == 0 {
		return "", false
	}
	return f[0], len(f) > 1 && f[len(f)-1] == unverifiedMarker
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func (b *Brain) Recall(ctx context.Context, query string, limit int) ([]app.BrainItem, error) {
	// `query` searches pages, `question` ranks facts by relevance; limit is per arm, so the merge is cut to limit below.
	out, err := b.call(ctx, "recall", map[string]any{"query": query, "question": truncate(query, 2000), "limit": limit})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Facts []struct {
			FactID     json.RawMessage `json:"fact_id"`
			Fact       string          `json:"fact"`
			Provenance string          `json:"provenance"`
		} `json:"facts"`
		Results []struct {
			Slug  string `json:"slug"`
			Title string `json:"title"`
			Chunk string `json:"chunk"`
		} `json:"results"`
	}
	// TODO: uncertain — the text may carry a trailing notice block after the JSON document, so only the first value is decoded; verify against a live gbrain
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode recall response: %w", err)
	}
	var items []app.BrainItem
	for _, f := range resp.Facts {
		src, unv := decodeProvenance(f.Provenance)
		items = append(items, app.BrainItem{Slug: idString(f.FactID), Text: f.Fact, Source: src, Unverified: unv})
	}
	for _, r := range resp.Results {
		// TODO: uncertain — search hits only report their origin page slug, so every page is labelled import/mybook (the only content scripts/brain-setup.sh imports)
		items = append(items, app.BrainItem{Slug: r.Slug, Title: r.Title, Text: r.Chunk, Source: pageSource})
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func (b *Brain) Remember(ctx context.Context, item app.BrainItem) (string, error) {
	fact := item.Text
	if item.Title != "" {
		fact = item.Title + ": " + item.Text
	}
	origin := "user_said"
	if item.Unverified {
		origin = "inferred"
	}
	out, err := b.call(ctx, "remember", map[string]any{
		"fact": fact, "provenance": encodeProvenance(item.Source, item.Unverified), "kind": "fact", "content_origin": origin,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.NewDecoder(strings.NewReader(out)).Decode(&resp); err != nil {
		return "", fmt.Errorf("decode remember response: %w", err)
	}
	id := idString(resp.ID)
	if id == "" {
		return "", errors.New("remember response has no id")
	}
	return id, nil
}

// Forget expires a fact by the id Remember or Recall returned; gbrain cannot forget an imported page this way.
func (b *Brain) Forget(ctx context.Context, slug string) error {
	_, err := b.call(ctx, "forget", map[string]any{"id": slug})
	return err
}

// call returns the tool's text. Errors carry gbrain's error code only: its messages can echo stored content.
func (b *Brain) call(ctx context.Context, verb string, args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode %s args: %w", verb, err)
	}
	res, err := b.c.Call(ctx, task.ToolCall{ID: "gbrain-" + verb, Name: ServerName + "." + verb, Args: raw})
	if err != nil {
		return "", fmt.Errorf("gbrain %s: %w", verb, err)
	}
	if res.IsError || bytes.HasPrefix(bytes.TrimSpace([]byte(res.Content)), []byte(`{"error"`)) {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(res.Content), &e)
		if e.Error == "" {
			e.Error = "tool_error"
		}
		return "", fmt.Errorf("gbrain %s failed: %s", verb, e.Error)
	}
	return res.Content, nil
}

func idString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
