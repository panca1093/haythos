package testfakes

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

// BrainTools serves brain.recall, brain.remember and brain.forget over a Brain, stamping provenance
// from the context the way the real local tools do; every other tool goes to the embedded Tools.
type BrainTools struct {
	*Tools
	Brain *Brain
}

func NewBrainTools(b *Brain, specs ...task.ToolSpec) *BrainTools {
	schema := json.RawMessage(`{"type":"object"}`)
	specs = append(specs,
		task.ToolSpec{Name: "brain.recall", InputSchema: schema},
		task.ToolSpec{Name: "brain.remember", InputSchema: schema},
		task.ToolSpec{Name: "brain.forget", InputSchema: schema},
	)
	return &BrainTools{Tools: NewTools(specs...), Brain: b}
}

func (t *BrainTools) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	switch call.Name {
	case "brain.recall", "brain.remember", "brain.forget":
	default:
		return t.Tools.Call(ctx, call)
	}
	var args struct {
		Slug  string `json:"slug"`
		Title string `json:"title"`
		Text  string `json:"text"`
		Query string `json:"query"`
	}
	t.mu.Lock()
	t.Called[call.Name]++
	t.mu.Unlock()
	fail := func(err error) (task.ToolResult, error) {
		return task.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}, nil
	}
	if err := json.Unmarshal(call.Args, &args); err != nil {
		return fail(err)
	}
	switch call.Name {
	case "brain.recall":
		items, err := t.Brain.Recall(ctx, args.Query, 3)
		if err != nil {
			return fail(err)
		}
		b, _ := json.Marshal(items)
		return task.ToolResult{CallID: call.ID, Content: string(b)}, nil
	case "brain.remember":
		src := app.BrainSourceFrom(ctx)
		slug, err := t.Brain.Remember(ctx, app.BrainItem{Slug: args.Slug, Title: args.Title, Text: args.Text, Source: src, Unverified: strings.HasPrefix(src, "auto/")})
		if err != nil {
			return fail(err)
		}
		return task.ToolResult{CallID: call.ID, Content: `{"slug":"` + slug + `"}`}, nil
	}
	if err := t.Brain.Forget(ctx, args.Slug); err != nil {
		return fail(err)
	}
	return task.ToolResult{CallID: call.ID, Content: "forgot " + args.Slug}, nil
}
