package testfakes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type Model struct {
	mu    sync.Mutex
	turns []task.AssistantTurn
	Steps int
}

func NewModel(turns ...task.AssistantTurn) *Model { return &Model{turns: turns} }

func (m *Model) Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Steps >= len(m.turns) {
		return task.AssistantTurn{}, errors.New("fake model: script exhausted")
	}
	t := m.turns[m.Steps]
	m.Steps++
	return t, nil
}

type Tools struct {
	mu       sync.Mutex
	Specs    []task.ToolSpec
	Handlers map[string]func(task.ToolCall) (task.ToolResult, error)
	Called   map[string]int
	ExistsFn func(tool, target, idemKey string) (bool, error)
}

func NewTools(specs ...task.ToolSpec) *Tools {
	return &Tools{Specs: specs, Handlers: map[string]func(task.ToolCall) (task.ToolResult, error){}, Called: map[string]int{}}
}

func (t *Tools) List(scope []string) []task.ToolSpec {
	var out []task.ToolSpec
	for _, s := range t.Specs {
		for _, n := range scope {
			if s.Name == n {
				out = append(out, s)
			}
		}
	}
	return out
}

func (t *Tools) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	t.mu.Lock()
	t.Called[call.Name]++
	h := t.Handlers[call.Name]
	t.mu.Unlock()
	if h == nil {
		return task.ToolResult{CallID: call.ID, Content: "ok"}, nil
	}
	r, err := h(call)
	r.CallID = call.ID
	return r, err
}

func (t *Tools) Exists(ctx context.Context, tool, target, idemKey string) (bool, error) {
	if t.ExistsFn == nil {
		return false, app.ErrCannotVerify
	}
	return t.ExistsFn(tool, target, idemKey)
}

func (t *Tools) Count(name string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Called[name]
}

type Notifier struct {
	mu       sync.Mutex
	Notices  []app.Notice
	FailNext int
}

func (n *Notifier) Publish(ctx context.Context, notice app.Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.FailNext > 0 {
		n.FailNext--
		return errors.New("fake notifier: publish failed")
	}
	n.Notices = append(n.Notices, notice)
	return nil
}

func (n *Notifier) Of(kind app.NoticeKind) []app.Notice {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []app.Notice
	for _, x := range n.Notices {
		if x.Kind == kind {
			out = append(out, x)
		}
	}
	return out
}

type Vault map[string]string

func (v Vault) Resolve(handle string) (string, error) {
	s, ok := v[handle]
	if !ok {
		return "", fmt.Errorf("resolve %s: not found", handle)
	}
	return s, nil
}

type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock(t time.Time) *Clock { return &Clock{t: t} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
