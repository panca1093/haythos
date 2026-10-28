package app_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
	"github.com/panca/haythos/internal/testfakes"
)

type perTargetNotifier struct {
	mu       sync.Mutex
	failFor  map[string]bool
	received []app.Notice
}

func (p *perTargetNotifier) Publish(_ context.Context, n app.Notice) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failFor[n.ReplyTo] {
		return context.DeadlineExceeded
	}
	p.received = append(p.received, n)
	return nil
}

func (p *perTargetNotifier) targets(kind app.NoticeKind) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, n := range p.received {
		if n.Kind == kind {
			out = append(out, n.ReplyTo)
		}
	}
	slices.Sort(out)
	return out
}

func deliveryApp(t *testing.T, db string, n app.Notifier, tools app.Tools, turns ...task.AssistantTurn) *app.App {
	t.Helper()
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.add", "jobs.define"}},
		Tools:      tools,
		Events:     st, Ledger: st, Notifier: n, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{
			"note.add": policy.TierInternalWrite, "jobs.define": policy.TierInternalWrite, "note.read": policy.TierRead,
		}},
		Clock:       func() time.Time { return time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC) },
		ApprovalTTL: time.Hour,
	})
}

func noteSpecs() []task.ToolSpec {
	return []task.ToolSpec{
		{Name: "note.add", Write: true, TargetArg: "id", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "note.read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "jobs.define", Write: true, TargetArg: "name", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
}

func TestEachTargetGetsEachNoticeOnceAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db := t.TempDir() + "/haythos.db"
	n := &perTargetNotifier{failFor: map[string]bool{"thread-2": true}}
	write := task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "note.add", Args: json.RawMessage(`{"id":"n1"}`)}}}
	job := app.Job{Name: "report", Goal: "g", Model: "default", Tools: []string{"note.add"}, DeliverTo: []string{"thread-1", "thread-2"}}

	a := deliveryApp(t, db, n, testfakes.NewTools(noteSpecs()...), write)
	id, err := a.CreateTask(ctx, job, "go")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := a.RunTask(ctx, id); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := n.targets(app.NoticeApproval); !slices.Equal(got, []string{"thread-1"}) {
		t.Fatalf("approval targets = %v, want only thread-1 (thread-2 failed)", got)
	}

	n.mu.Lock()
	n.failFor = nil
	n.mu.Unlock()
	restarted := deliveryApp(t, db, n, testfakes.NewTools(noteSpecs()...), write)
	if err := restarted.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if err := restarted.Tick(ctx); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := n.targets(app.NoticeApproval); !slices.Equal(got, []string{"thread-1", "thread-2"}) {
		t.Fatalf("approval targets = %v, want each target exactly once", got)
	}
}

func TestTargetsAndReplyToStayReadableForOldEvents(t *testing.T) {
	var c task.Created
	if err := json.Unmarshal([]byte(`{"job":"chat","reply_to":"dm-1"}`), &c); err != nil || c.ReplyTo != "dm-1" || len(c.Targets) != 0 {
		t.Fatalf("old created event = %+v, %v", c, err)
	}
}

type defineTools struct {
	*testfakes.Tools
	a   *app.App
	def app.JobDef
}

func (d *defineTools) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	if call.Name != "jobs.define" {
		return d.Tools.Call(ctx, call)
	}
	return task.ToolResult{CallID: call.ID, Content: "ok"}, d.a.DefineJob(ctx, d.def)
}

func TestDefineJobDefaultsDeliveryToTheDefiningTasksOrigin(t *testing.T) {
	tests := []struct {
		name      string
		deliverTo []string
		replyTo   string
		want      []string
	}{
		{name: "empty uses origin", replyTo: "thread-9", want: []string{"thread-9"}},
		{name: "explicit wins", deliverTo: []string{"chan-1", "dm"}, replyTo: "thread-9", want: []string{"chan-1", "dm"}},
		{name: "duplicates and blanks dropped", deliverTo: []string{"chan-1", " chan-1 ", ""}, replyTo: "thread-9", want: []string{"chan-1"}},
		{name: "no origin and no targets stays empty", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			n := &testfakes.Notifier{}
			tools := &defineTools{Tools: testfakes.NewTools(noteSpecs()...), def: app.JobDef{Name: "disk", Cron: "0 9 * * 1", Goal: "check", Tools: []string{"note.read"}, DeliverTo: tt.deliverTo}}
			a := deliveryApp(t, t.TempDir()+"/haythos.db", n, tools,
				task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "jobs.define", Args: json.RawMessage(`{"name":"disk"}`)}}},
				task.AssistantTurn{Text: "done"})
			tools.a = a
			id, err := a.HandleInput(ctx, app.Input{Text: "define it", ReplyTo: tt.replyTo})
			if err != nil {
				t.Fatalf("handle: %v", err)
			}
			asks := n.Of(app.NoticeApproval)
			if len(asks) != 1 {
				t.Fatalf("approvals = %d", len(asks))
			}
			if err := a.HandleApproval(ctx, id, asks[0].Approval.Approval.ID, true); err != nil {
				t.Fatalf("approve: %v", err)
			}
			js, err := a.ListJobs(ctx)
			if err != nil || len(js) != 1 {
				t.Fatalf("jobs = %+v, %v", js, err)
			}
			if !slices.Equal(js[0].DeliverTo, tt.want) {
				t.Errorf("deliver_to = %v, want %v", js[0].DeliverTo, tt.want)
			}
		})
	}
}

func TestScheduledJobDeliversToItsTargetsAfterRestart(t *testing.T) {
	ctx := context.Background()
	db := t.TempDir() + "/haythos.db"
	now := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	clock := testfakes.NewClock(now)
	n := &perTargetNotifier{}
	build := func(turns ...task.AssistantTurn) *app.App {
		st, err := sqlite.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return app.New(app.Deps{
			Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
			DefaultJob: app.Job{Name: "chat", Goal: "g", Model: "default"},
			Tools:      testfakes.NewTools(noteSpecs()...),
			Events:     st, Ledger: st, Notifier: n, Vault: testfakes.Vault{},
			Policy: policy.Policy{Tiers: map[string]policy.Tier{"note.read": policy.TierRead}},
			Clock:  clock.Now,
		})
	}
	a := build(task.AssistantTurn{Text: "report"})
	if err := a.DefineJob(ctx, app.JobDef{Name: "disk", Cron: "0 10 * * *", Goal: "check", Tools: []string{"note.read"}, DeliverTo: []string{"thread-1", "dm"}}); err != nil {
		t.Fatalf("define: %v", err)
	}
	clock.Advance(24 * time.Hour)
	restarted := build(task.AssistantTurn{Text: "report"})
	if err := restarted.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := n.targets(app.NoticeMessage); !slices.Equal(got, []string{"dm", "thread-1"}) {
		t.Fatalf("message targets = %v", got)
	}
}
