package testfakes

import (
	"context"
	"encoding/json"
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
	// Histories holds the history passed to each Step call, in order.
	Histories [][]task.Message
}

func NewModel(turns ...task.AssistantTurn) *Model { return &Model{turns: turns} }

func (m *Model) Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Histories = append(m.Histories, history)
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

type ModelSelector struct {
	mu        sync.Mutex
	Choices   []app.ModelChoice
	Cur       string
	ListErr   error
	SelectErr error
}

func (m *ModelSelector) List(ctx context.Context) ([]app.ModelChoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ListErr != nil {
		return nil, m.ListErr
	}
	return append([]app.ModelChoice(nil), m.Choices...), nil
}

func (m *ModelSelector) Current() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Cur
}

func (m *ModelSelector) Select(ctx context.Context, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SelectErr != nil {
		return m.SelectErr
	}
	m.Cur = value
	return nil
}

type Events struct {
	mu        sync.Mutex
	Appended  []task.Event
	AppendErr error
}

func (e *Events) Append(ctx context.Context, taskID string, typ task.EventType, payload any) (task.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.AppendErr != nil {
		return task.Event{}, e.AppendErr
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return task.Event{}, fmt.Errorf("marshal payload: %w", err)
	}
	ev := task.Event{Cursor: int64(len(e.Appended) + 1), TaskID: taskID, Type: typ, Payload: raw}
	e.Appended = append(e.Appended, ev)
	return ev, nil
}

func (e *Events) Load(ctx context.Context, taskID string) ([]task.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []task.Event
	for _, ev := range e.Appended {
		if ev.TaskID == taskID {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (e *Events) Since(ctx context.Context, cursor int64) ([]task.Event, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []task.Event
	for _, ev := range e.Appended {
		if ev.Cursor > cursor {
			out = append(out, ev)
		}
	}
	return out, nil
}

func (e *Events) TasksIn(ctx context.Context, state task.State) ([]string, error) { return nil, nil }

type Ledger struct {
	mu   sync.Mutex
	rows map[string]task.LedgerEntry
}

func NewLedger() *Ledger { return &Ledger{rows: map[string]task.LedgerEntry{}} }

func (l *Ledger) Get(ctx context.Context, idemKey string) (task.LedgerEntry, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.rows[idemKey]
	return e, ok, nil
}

func (l *Ledger) Put(ctx context.Context, e task.LedgerEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows[e.IdemKey] = e
	return nil
}

func (l *Ledger) InState(ctx context.Context, s task.LedgerState) ([]task.LedgerEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []task.LedgerEntry
	for _, e := range l.rows {
		if e.State == s {
			out = append(out, e)
		}
	}
	return out, nil
}
