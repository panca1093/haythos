package local

import (
	"context"
	"fmt"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type Handler func(ctx context.Context, args []byte) (task.ToolResult, error)

// Registry holds Go-native tools. It implements app.Tools.
type Registry struct {
	specs    []task.ToolSpec
	handlers map[string]Handler
}

func New() *Registry { return &Registry{handlers: map[string]Handler{}} }

func (r *Registry) Register(spec task.ToolSpec, h Handler) {
	r.specs = append(r.specs, spec)
	r.handlers[spec.Name] = h
}

func (r *Registry) List(scope []string) []task.ToolSpec {
	var out []task.ToolSpec
	for _, s := range r.specs {
		for _, n := range scope {
			if s.Name == n {
				out = append(out, s)
			}
		}
	}
	return out
}

func (r *Registry) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	h, ok := r.handlers[call.Name]
	if !ok {
		return task.ToolResult{}, fmt.Errorf("unknown local tool %q", call.Name)
	}
	return h(ctx, call.Args)
}

// Exists: local tools make no external writes, so there is never a destination to check.
func (r *Registry) Exists(ctx context.Context, tool, target, idemKey string) (bool, error) {
	return false, app.ErrCannotVerify
}
