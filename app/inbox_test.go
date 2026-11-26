package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

// gateModel blocks every Step until release or ctx; started gets the step's newest user text.
type gateModel struct {
	mu        sync.Mutex
	histories [][]task.Message
	started   chan string
	release   chan task.AssistantTurn
}

func newGateModel() *gateModel {
	return &gateModel{started: make(chan string, 16), release: make(chan task.AssistantTurn)}
}

func (m *gateModel) Step(ctx context.Context, history []task.Message, _ []task.ToolSpec) (task.AssistantTurn, error) {
	m.mu.Lock()
	m.histories = append(m.histories, history)
	m.mu.Unlock()
	var user string
	for _, h := range history {
		if h.Role == task.RoleUser && !h.Context {
			user = h.Text
		}
	}
	m.started <- user
	select {
	case t := <-m.release:
		return t, nil
	case <-ctx.Done():
		return task.AssistantTurn{}, ctx.Err()
	}
}

func (m *gateModel) history(i int) []task.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.histories[i]
}

type inboxRig struct {
	app      *app.App
	events   *testfakes.Events
	ledger   *testfakes.Ledger
	notifier *testfakes.Notifier
	tools    *testfakes.Tools
}

func newInboxRig(t *testing.T, model app.Model, pol policy.Policy) *inboxRig {
	t.Helper()
	r := &inboxRig{events: &testfakes.Events{}, ledger: testfakes.NewLedger(), notifier: &testfakes.Notifier{},
		tools: testfakes.NewTools(task.ToolSpec{Name: "note.write", Write: true, TargetArg: "to", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	r.app = app.New(app.Deps{
		Models:     map[string]app.Model{"default": model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.write"}},
		Tools:      r.tools, Events: r.events, Ledger: r.ledger, Notifier: r.notifier, Vault: testfakes.Vault{},
		Policy: pol, Clock: func() time.Time { return time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC) },
	})
	return r
}

type sent struct {
	id  string
	err error
}

func (r *inboxRig) send(replyTo, text string) chan sent {
	out := make(chan sent, 1)
	go func() {
		id, err := r.app.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: replyTo})
		out <- sent{id, err}
	}()
	return out
}

// waitQueued waits until the inbox of replyTo holds n messages.
func waitQueued(t *testing.T, events app.EventStore, replyTo string, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		evs, _ := events.Load(context.Background(), "inbox:"+replyTo)
		got := 0
		for _, e := range evs {
			if e.Type == "inbox.msg" {
				got++
			}
		}
		if got >= n {
			return
		}
	}
	t.Fatalf("inbox %s never held %d messages", replyTo, n)
}

func recv[T any](t *testing.T, ch chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}
	var zero T
	return zero
}

func snap(t *testing.T, a *app.App, id string) task.Snapshot {
	t.Helper()
	s, err := a.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInboxSerializesAndCoalescesALane(t *testing.T) {
	m := newGateModel()
	r := newInboxRig(t, m, policy.Policy{Tiers: map[string]policy.Tier{}})
	first := r.send("c1", "one")
	if got := recv(t, m.started); got != "one" {
		t.Fatalf("first turn = %q, want one", got)
	}
	second := r.send("c1", "two")
	waitQueued(t, r.events, "c1", 2)
	third := r.send("c1", "three")
	waitQueued(t, r.events, "c1", 3)
	m.release <- task.AssistantTurn{Text: "first answer"}
	if got := recv(t, m.started); got != "two\n\nthree" {
		t.Fatalf("second turn = %q, want the two queued messages coalesced", got)
	}
	if h := texts(m.history(1)); !strings.Contains(h, "user:one|assistant:first answer|") {
		t.Errorf("second turn history = %s, want the first exchange as context", h)
	}
	m.release <- task.AssistantTurn{Text: "second answer"}
	s1, s2, s3 := recv(t, first), recv(t, second), recv(t, third)
	for _, s := range []sent{s1, s2, s3} {
		if s.err != nil {
			t.Fatal(s.err)
		}
	}
	if s1.id == s2.id || s2.id != s3.id {
		t.Fatalf("task ids = %s %s %s, want the last two shared and the first distinct", s1.id, s2.id, s3.id)
	}
	for _, id := range []string{s1.id, s2.id} {
		if st := snap(t, r.app, id).State; st != task.Done {
			t.Errorf("task %s is %s, want done", id, st)
		}
	}
	select {
	case got := <-m.started:
		t.Fatalf("unexpected third turn %q", got)
	default:
	}
}

func TestInboxLanesRunConcurrently(t *testing.T) {
	m := newGateModel()
	r := newInboxRig(t, m, policy.Policy{Tiers: map[string]policy.Tier{}})
	a, b := r.send("c1", "a"), r.send("c2", "b")
	got := map[string]bool{recv(t, m.started): true, recv(t, m.started): true}
	if !got["a"] || !got["b"] {
		t.Fatalf("started turns = %v, want a and b both running", got)
	}
	m.release <- task.AssistantTurn{Text: "x"}
	m.release <- task.AssistantTurn{Text: "y"}
	if s := recv(t, a); s.err != nil {
		t.Fatal(s.err)
	}
	if s := recv(t, b); s.err != nil {
		t.Fatal(s.err)
	}
}

func TestInterruptStopsTheTurnAndRunsTheQueue(t *testing.T) {
	m := newGateModel()
	r := newInboxRig(t, m, policy.Policy{Tiers: map[string]policy.Tier{}})
	ctx := context.Background()
	if err := r.app.Interrupt(ctx, "c1"); !errors.Is(err, app.ErrNothingRunning) {
		t.Fatalf("interrupt on an idle lane = %v, want ErrNothingRunning", err)
	}
	first := r.send("c1", "build it with mysql")
	recv(t, m.started)
	queued := []chan sent{r.send("c1", "use postgres")}
	waitQueued(t, r.events, "c1", 2)
	queued = append(queued, r.send("c1", "and redis"))
	waitQueued(t, r.events, "c1", 3)

	if err := r.app.Interrupt(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	stopped := recv(t, first)
	if stopped.err != nil {
		t.Fatal(stopped.err)
	}
	if s := snap(t, r.app, stopped.id); s.State != task.Failed || s.Reason != "interrupted" {
		t.Fatalf("stopped task = %s (%s), want failed: interrupted", s.State, s.Reason)
	}
	if got := recv(t, m.started); got != "use postgres\n\nand redis" {
		t.Fatalf("next turn = %q, want the queued messages coalesced", got)
	}
	if h := texts(m.history(1)); !strings.Contains(h, "user:build it with mysql|assistant:[stopped by the owner before finishing]|") {
		t.Errorf("next turn history = %s, want the stopped request with the stopped marker", h)
	}
	m.release <- task.AssistantTurn{Text: "postgres and redis it is"}
	next := recv(t, queued[0])
	if next.err != nil || recv(t, queued[1]).id != next.id {
		t.Fatalf("queued messages did not share one turn: %+v", next)
	}

	var stopNotices int
	for _, n := range r.notifier.Notices {
		if n.TaskID == stopped.id && n.Text == "Stopped." {
			stopNotices++
		}
		if n.TaskID == stopped.id && strings.HasPrefix(n.Text, "Task failed") {
			t.Errorf("stopped task sent a failure notice %q", n.Text)
		}
	}
	if stopNotices != 1 {
		t.Errorf("Stopped. notices = %d, want 1", stopNotices)
	}
	brain, _ := r.events.Load(ctx, "_brain")
	for _, e := range brain {
		if strings.Contains(string(e.Payload), stopped.id) {
			t.Errorf("brain marker for the stopped task: %s", e.Payload)
		}
	}
	inbox, _ := r.events.Load(ctx, "inbox:c1")
	var interrupts int
	for _, e := range inbox {
		if e.Type == "inbox.interrupt" {
			interrupts++
		}
	}
	if interrupts != 1 {
		t.Errorf("inbox.interrupt events = %d, want 1", interrupts)
	}
}

func TestInterruptKeepsALedgerWriteInFlight(t *testing.T) {
	m := testfakes.NewModel(task.AssistantTurn{Calls: []task.ToolCall{{ID: "w1", Name: "note.write", Args: json.RawMessage(`{"to":"notes"}`)}}})
	r := newInboxRig(t, m, policy.Policy{Tiers: map[string]policy.Tier{"note.write": policy.TierInternalWrite}, AutoWhenClean: []string{"note.write"}})
	dispatched, unblock := make(chan struct{}), make(chan struct{})
	r.tools.Handlers["note.write"] = func(c task.ToolCall) (task.ToolResult, error) {
		close(dispatched)
		<-unblock
		return task.ToolResult{Content: "written"}, nil
	}
	done := r.send("c1", "write a note")
	recv(t, dispatched)
	if err := r.app.Interrupt(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	close(unblock)
	s := recv(t, done)
	if s.err != nil {
		t.Fatal(s.err)
	}
	if st := snap(t, r.app, s.id); st.State != task.Failed || st.Reason != "interrupted" {
		t.Fatalf("task = %s (%s), want failed: interrupted", st.State, st.Reason)
	}
	row, ok, _ := r.ledger.Get(context.Background(), s.id+"/w1")
	if !ok || row.State != task.LedgerSucceeded {
		t.Fatalf("ledger row = %+v (found %v), want the write to keep its succeeded state", row, ok)
	}
	if m.Steps != 1 {
		t.Errorf("model steps = %d, want no step after the interrupt", m.Steps)
	}
}

func TestApprovalReleasesTheLane(t *testing.T) {
	m := testfakes.NewModel(
		task.AssistantTurn{Calls: []task.ToolCall{{ID: "w1", Name: "note.write", Args: json.RawMessage(`{"to":"notes"}`)}}},
		task.AssistantTurn{Text: "hello"},
	)
	r := newInboxRig(t, m, policy.Policy{Tiers: map[string]policy.Tier{"note.write": policy.TierInternalWrite}})
	first := recv(t, r.send("c1", "write a note"))
	if first.err != nil {
		t.Fatal(first.err)
	}
	if st := snap(t, r.app, first.id).State; st != task.WaitingApproval {
		t.Fatalf("first task is %s, want waiting_approval", st)
	}
	second := recv(t, r.send("c1", "hi"))
	if second.err != nil {
		t.Fatal(second.err)
	}
	if second.id == first.id || snap(t, r.app, second.id).State != task.Done {
		t.Fatalf("second message did not run as its own finished task: %s", second.id)
	}
}

func TestInboxResumesAfterACrashBeforeAck(t *testing.T) {
	db := t.TempDir() + "/haythos.db"
	open := func(m app.Model, hook func(string) error) (*app.App, *sqlite.Store) {
		st, err := sqlite.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return app.New(app.Deps{
			Models:     map[string]app.Model{"default": m},
			DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default"},
			Tools:      testfakes.NewTools(), Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
			Policy: policy.Policy{Tiers: map[string]policy.Tier{}}, Hook: hook,
		}), st
	}
	crash := errors.New("simulated crash")
	a1, _ := open(testfakes.NewModel(task.AssistantTurn{Text: "answer"}), func(p string) error {
		if p == "inbox.before_ack" {
			return crash
		}
		return nil
	})
	if _, err := a1.HandleInput(context.Background(), app.Input{Text: "hi", ReplyTo: "c1"}); !errors.Is(err, crash) {
		t.Fatalf("handle input = %v, want the simulated crash", err)
	}

	a2, st := open(testfakes.NewModel(), nil)
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var acks []string
	for deadline := time.Now().Add(5 * time.Second); len(acks) == 0 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		evs, err := st.Load(context.Background(), "inbox:c1")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Type == "inbox.ack" {
				var ack struct{ Task string }
				_ = json.Unmarshal(e.Payload, &ack)
				acks = append(acks, ack.Task)
			}
		}
	}
	all, err := st.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var created []string
	for _, e := range all {
		if e.Type == task.EvCreated {
			created = append(created, e.TaskID)
		}
	}
	if len(created) != 1 || len(acks) != 1 || acks[0] != created[0] {
		t.Fatalf("tasks %v, acks %v; want one task and one ack for it", created, acks)
	}
	if s := snap(t, a2, created[0]); s.State != task.Done {
		t.Fatalf("task is %s, want done", s.State)
	}
}
