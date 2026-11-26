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
	stopped := interruptAsync(r.app, "c1")
	close(unblock)
	if err := recv(t, stopped); err != nil {
		t.Fatal(err)
	}
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

// openSQLiteApp builds an App on the sqlite file db, as a restarted kernel would.
func openSQLiteApp(t *testing.T, db string, m app.Model, tools app.Tools, pol policy.Policy, hook func(string) error) (*app.App, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": m},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.read", "note.write"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: pol, Hook: hook,
	}), st
}

func crashAt(point string, crash error) func(string) error {
	return func(p string) error {
		if p == point {
			return crash
		}
		return nil
	}
}

// inboxAcks returns the task of every inbox.ack on the lane replyTo, in order.
func inboxAcks(t *testing.T, events app.EventStore, replyTo string) []string {
	t.Helper()
	evs, err := events.Load(context.Background(), "inbox:"+replyTo)
	if err != nil {
		t.Fatal(err)
	}
	var acks []string
	for _, e := range evs {
		if e.Type == "inbox.ack" {
			var ack struct{ Task string }
			_ = json.Unmarshal(e.Payload, &ack)
			acks = append(acks, ack.Task)
		}
	}
	return acks
}

// userTasks maps every task id to the user text it was created with.
func userTasks(t *testing.T, events app.EventStore) map[string]string {
	t.Helper()
	all, err := events.Since(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	created := map[string]bool{}
	for _, e := range all {
		switch e.Type {
		case task.EvCreated:
			created[e.TaskID] = true
		case task.EvMessage:
			var m task.Message
			if created[e.TaskID] && json.Unmarshal(e.Payload, &m) == nil && m.Role == task.RoleUser && !m.Context {
				out[e.TaskID] = m.Text
			}
		}
	}
	return out
}

func TestInboxResumesAfterACrashBeforeAck(t *testing.T) {
	db := t.TempDir() + "/haythos.db"
	read := task.ToolSpec{Name: "note.read", InputSchema: json.RawMessage(`{"type":"object"}`)}
	pol := policy.Policy{Tiers: map[string]policy.Tier{}}
	crash := errors.New("simulated crash")
	a1, _ := openSQLiteApp(t, db, testfakes.NewModel(task.AssistantTurn{Calls: []task.ToolCall{{ID: "r1", Name: "note.read", Args: json.RawMessage(`{}`)}}}),
		testfakes.NewTools(read), pol, crashAt("app.after_tool_result", crash))
	if _, err := a1.HandleInput(context.Background(), app.Input{Text: "hi", ReplyTo: "c1"}); !errors.Is(err, crash) {
		t.Fatalf("handle input = %v, want the simulated crash", err)
	}

	a2, st := openSQLiteApp(t, db, testfakes.NewModel(task.AssistantTurn{Text: "answer"}), testfakes.NewTools(read), pol, nil)
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2.Wait()
	tasks := userTasks(t, st)
	acks := inboxAcks(t, st, "c1")
	if len(tasks) != 1 || len(acks) != 1 || tasks[acks[0]] != "hi" {
		t.Fatalf("tasks %v, acks %v; want one task and one ack for it", tasks, acks)
	}
	if s := snap(t, a2, acks[0]); s.State != task.Done {
		t.Fatalf("task is %s, want done", s.State)
	}
}

func TestInboxCrashKeepsTheCoalescingBoundary(t *testing.T) {
	db := t.TempDir() + "/haythos.db"
	pol := policy.Policy{Tiers: map[string]policy.Tier{}}
	crash := errors.New("simulated crash")
	m := newGateModel()
	a1, st1 := openSQLiteApp(t, db, m, testfakes.NewTools(), pol, crashAt("inbox.before_ack", crash))
	send := func(text string) chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := a1.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: "c1"})
			ch <- err
		}()
		return ch
	}
	one := send("one")
	if got := recv(t, m.started); got != "one" {
		t.Fatalf("first turn = %q, want one", got)
	}
	two := send("two")
	waitQueued(t, st1, "c1", 2)
	m.release <- task.AssistantTurn{Text: "answer one"}
	for _, ch := range []chan error{one, two} {
		if err := recv(t, ch); !errors.Is(err, crash) {
			t.Fatalf("handle input = %v, want the simulated crash", err)
		}
	}

	a2, st := openSQLiteApp(t, db, testfakes.NewModel(task.AssistantTurn{Text: "answer two"}), testfakes.NewTools(), pol, nil)
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2.Wait()
	acks := inboxAcks(t, st, "c1")
	tasks := userTasks(t, st)
	if len(acks) != 2 || tasks[acks[0]] != "one" || tasks[acks[1]] != "two" {
		t.Fatalf("acks %v over tasks %v; want one ack for the task of one, then one for two alone", acks, tasks)
	}
	ones := 0
	for _, text := range tasks {
		if strings.Contains(text, "one") {
			ones++
		}
	}
	inbox, _ := st.Load(context.Background(), "inbox:c1")
	takes := 0
	for _, e := range inbox {
		if e.Type == "inbox.take" {
			takes++
		}
	}
	if takes != len(acks) {
		t.Errorf("inbox.take events = %d, want one per ack (%d)", takes, len(acks))
	}
	if ones != 1 || len(tasks) != 2 {
		t.Fatalf("tasks %v; want exactly one task with one and one with two", tasks)
	}
	for _, id := range acks {
		if s := snap(t, a2, id); s.State != task.Done {
			t.Errorf("task %s is %s, want done", id, s.State)
		}
	}
}

func interruptAsync(a *app.App, replyTo string) chan error {
	ch := make(chan error, 1)
	go func() { ch <- a.Interrupt(context.Background(), replyTo) }()
	return ch
}

// ctxTools blocks every Call until release or its ctx ends, like a real tool honoring cancellation.
type ctxTools struct {
	*testfakes.Tools
	dispatched, release chan struct{}
}

func (c ctxTools) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	close(c.dispatched)
	select {
	case <-c.release:
		return task.ToolResult{CallID: call.ID, Content: "written"}, nil
	case <-ctx.Done():
		return task.ToolResult{CallID: call.ID}, ctx.Err()
	}
}

func TestInterruptLetsADispatchFinishOnTheLedger(t *testing.T) {
	m := testfakes.NewModel(task.AssistantTurn{Calls: []task.ToolCall{{ID: "w1", Name: "note.write", Args: json.RawMessage(`{"to":"notes"}`)}}})
	tools := ctxTools{testfakes.NewTools(task.ToolSpec{Name: "note.write", Write: true, TargetArg: "to", InputSchema: json.RawMessage(`{"type":"object"}`)}), make(chan struct{}), make(chan struct{})}
	pol := policy.Policy{Tiers: map[string]policy.Tier{"note.write": policy.TierInternalWrite}, AutoWhenClean: []string{"note.write"}}
	a, st := openSQLiteApp(t, t.TempDir()+"/haythos.db", m, tools, pol, nil)
	done := make(chan sent, 1)
	go func() {
		id, err := a.HandleInput(context.Background(), app.Input{Text: "write a note", ReplyTo: "c1"})
		done <- sent{id, err}
	}()
	recv(t, tools.dispatched)
	stopped := interruptAsync(a, "c1")
	close(tools.release)
	if err := recv(t, stopped); err != nil {
		t.Fatal(err)
	}
	s := recv(t, done)
	if s.err != nil {
		t.Fatal(s.err)
	}
	if row, ok, err := st.Get(context.Background(), s.id+"/w1"); err != nil || !ok || row.State != task.LedgerSucceeded {
		t.Fatalf("ledger row = %+v (found %v, err %v), want the dispatch recorded as succeeded", row, ok, err)
	}
	sn := snap(t, a, s.id)
	if sn.State != task.Failed || sn.Reason != "interrupted" {
		t.Fatalf("task = %s (%s), want failed: interrupted", sn.State, sn.Reason)
	}
	var result bool
	for _, h := range sn.History {
		for _, r := range h.Results {
			result = result || (r.CallID == "w1" && r.Content == "written")
		}
	}
	if !result {
		t.Errorf("history has no result for w1")
	}
	if m.Steps != 1 {
		t.Errorf("model steps = %d, want no step after the interrupt", m.Steps)
	}
}

func TestStartFinishesAStopInterruptedByACrash(t *testing.T) {
	db := t.TempDir() + "/haythos.db"
	pol := policy.Policy{Tiers: map[string]policy.Tier{}}
	crash := errors.New("simulated crash")
	m := newGateModel()
	a1, _ := openSQLiteApp(t, db, m, testfakes.NewTools(), pol, crashAt("inbox.before_stop_state", crash))
	done := make(chan error, 1)
	go func() {
		_, err := a1.HandleInput(context.Background(), app.Input{Text: "one", ReplyTo: "c1"})
		done <- err
	}()
	recv(t, m.started)
	if err := a1.Interrupt(context.Background(), "c1"); !errors.Is(err, crash) {
		t.Fatalf("interrupt = %v, want the simulated crash", err)
	}
	if err := recv(t, done); !errors.Is(err, crash) {
		t.Fatalf("handle input = %v, want the simulated crash", err)
	}

	m2 := testfakes.NewModel()
	a2, st := openSQLiteApp(t, db, m2, testfakes.NewTools(), pol, nil)
	if err := a2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2.Wait()
	id := inboxAcks(t, st, "c1")[0]
	if s := snap(t, a2, id); s.State != task.Failed || s.Reason != "interrupted" {
		t.Fatalf("task = %s (%s), want failed: interrupted", s.State, s.Reason)
	}
	if m2.Steps != 0 {
		t.Errorf("model steps after restart = %d, want the stopped task not resumed", m2.Steps)
	}
	conv, _ := st.Load(context.Background(), "conv:c1")
	turns := 0
	for _, e := range conv {
		if e.Type == "conv.turn" && strings.Contains(string(e.Payload), id) {
			turns++
		}
	}
	if turns != 1 {
		t.Errorf("conv.turn events for %s = %d, want 1", id, turns)
	}
	evs, _ := st.Load(context.Background(), id)
	stops := 0
	for _, e := range evs {
		if e.Type == task.EvNoticeSent && strings.Contains(string(e.Payload), `"stopped"`) {
			stops++
		}
	}
	if stops != 1 {
		t.Errorf("stopped notices = %d, want 1", stops)
	}
}

func TestShutdownStopsALaneTurnAndWaitReturns(t *testing.T) {
	m := newGateModel()
	r := newInboxRig(t, m, policy.Policy{Tiers: map[string]policy.Tier{}})
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	if err := r.app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	done := r.send("c1", "one")
	recv(t, m.started)
	shutdown()
	if s := recv(t, done); !errors.Is(s.err, context.Canceled) {
		t.Fatalf("handle input = %v, want the turn stopped by shutdown", s.err)
	}
	waited := make(chan struct{})
	go func() { r.app.Wait(); close(waited) }()
	recv(t, waited)
	inbox, _ := r.events.Load(context.Background(), "inbox:c1")
	var take struct{ Task string }
	for _, e := range inbox {
		if e.Type == "inbox.take" {
			_ = json.Unmarshal(e.Payload, &take)
		}
	}
	if s := snap(t, r.app, take.Task); s.State != task.Running {
		t.Errorf("task is %s, want running so the next Start resumes it", s.State)
	}
}

// gatedCreate blocks the append of a task's created event until release, as a slow conversation load would.
type gatedCreate struct {
	*testfakes.Events
	reached, release chan struct{}
}

func (g gatedCreate) Append(ctx context.Context, taskID string, typ task.EventType, payload any) (task.Event, error) {
	if typ == task.EvCreated {
		close(g.reached)
		<-g.release
	}
	return g.Events.Append(ctx, taskID, typ, payload)
}

func TestInterruptBeforeTheTaskExistsStopsIt(t *testing.T) {
	m := newGateModel()
	events := gatedCreate{&testfakes.Events{}, make(chan struct{}), make(chan struct{})}
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": m},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default"},
		Tools:      testfakes.NewTools(), Events: events, Ledger: testfakes.NewLedger(), Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{}},
	})
	done := make(chan sent, 1)
	go func() {
		id, err := a.HandleInput(context.Background(), app.Input{Text: "one", ReplyTo: "c1"})
		done <- sent{id, err}
	}()
	recv(t, events.reached)
	requested, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Interrupt(requested, "c1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupt = %v, want the stop requested and the wait cut short", err)
	}
	close(events.release)
	s := recv(t, done)
	if s.err != nil {
		t.Fatal(s.err)
	}
	if st := snap(t, a, s.id); st.State != task.Failed || st.Reason != "interrupted" {
		t.Fatalf("task = %s (%s), want failed: interrupted", st.State, st.Reason)
	}
	select {
	case got := <-m.started:
		t.Fatalf("model ran %q after the stop", got)
	default:
	}
}

func TestInterruptAsTheTurnFinishesStopsNothing(t *testing.T) {
	var a *app.App
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	late, waited := make(chan error, 1), make(chan error, 1)
	events := &testfakes.Events{}
	a = app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(task.AssistantTurn{Text: "done"})},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default"},
		Tools:      testfakes.NewTools(), Events: events, Ledger: testfakes.NewLedger(), Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{}},
		Hook: func(p string) error {
			if p == "inbox.after_run" {
				late <- a.Interrupt(ctx, "c1")
				go func() { waited <- a.Interrupt(context.Background(), "c1") }()
			}
			return nil
		},
	})
	id, err := a.HandleInput(context.Background(), app.Input{Text: "one", ReplyTo: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := recv(t, late); !errors.Is(err, context.Canceled) {
		t.Fatalf("late interrupt = %v, want the wait cut short by its ctx", err)
	}
	if err := recv(t, waited); !errors.Is(err, app.ErrNothingRunning) {
		t.Fatalf("interrupt waiting on the finishing turn = %v, want ErrNothingRunning", err)
	}
	if st := snap(t, a, id).State; st != task.Done {
		t.Fatalf("task is %s, want done", st)
	}
	inbox, _ := events.Load(context.Background(), "inbox:c1")
	for _, e := range inbox {
		if e.Type == "inbox.interrupt" {
			t.Fatalf("inbox.interrupt written for a finished turn: %s", e.Payload)
		}
	}
	if err := a.Interrupt(context.Background(), "c1"); !errors.Is(err, app.ErrNothingRunning) {
		t.Fatalf("interrupt after the turn = %v, want ErrNothingRunning", err)
	}
}
