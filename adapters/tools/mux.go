// Package tools combines several app.Tools implementations into one.
package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

// Mux routes each tool name to the first member that lists it.
type Mux []app.Tools

var _ app.Tools = Mux(nil)

func (m Mux) List(scope []string) []task.ToolSpec {
	var out []task.ToolSpec
	for _, t := range m {
		out = append(out, t.List(scope)...)
	}
	slices.SortStableFunc(out, func(a, b task.ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (m Mux) owner(name string) (app.Tools, bool) {
	for _, t := range m {
		if len(t.List([]string{name})) > 0 {
			return t, true
		}
	}
	return nil, false
}

func (m Mux) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	t, ok := m.owner(call.Name)
	if !ok {
		return task.ToolResult{}, fmt.Errorf("unknown tool %q", call.Name)
	}
	return t.Call(ctx, call)
}

func (m Mux) Exists(ctx context.Context, tool, target, idemKey string) (bool, error) {
	t, ok := m.owner(tool)
	if !ok {
		return false, app.ErrCannotVerify
	}
	return t.Exists(ctx, tool, target, idemKey)
}
