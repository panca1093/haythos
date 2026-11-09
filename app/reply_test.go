package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func replyApp(t *testing.T, db string, n *testfakes.Notifier, turns ...task.AssistantTurn) *app.App {
	t.Helper()
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.add"}},
		Tools:      testfakes.NewTools(task.ToolSpec{Name: "note.add", Write: true, TargetArg: "id", InputSchema: json.RawMessage(`{"type":"object"}`)}),
		Events:     st, Ledger: st, Notifier: n, Vault: testfakes.Vault{},
		Policy:      policy.Policy{Tiers: map[string]policy.Tier{"note.add": policy.TierInternalWrite}},
		Clock:       func() time.Time { return time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC) },
		ApprovalTTL: time.Hour,
	})
}

func TestNoticesGoWhereTheTaskStartedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db := t.TempDir() + "/haythos.db"
	n := &testfakes.Notifier{}
	write := task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "note.add", Args: json.RawMessage(`{"id":"n1"}`)}}}

	id, err := replyApp(t, db, n, write).HandleInput(ctx, app.Input{Text: "add a note", ReplyTo: "dm-1"})
	if err != nil {
		t.Fatalf("handle input: %v", err)
	}
	asks := n.Of(app.NoticeApproval)
	if len(asks) != 1 || asks[0].ReplyTo != "dm-1" {
		t.Fatalf("approval notices = %+v, want one to dm-1", asks)
	}

	restarted := replyApp(t, db, n, task.AssistantTurn{Text: "noted"})
	if err := restarted.HandleApproval(ctx, id, asks[0].Approval.Approval.ID, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	done := n.Of(app.NoticeMessage)
	if len(done) != 1 || done[0].ReplyTo != "dm-1" {
		t.Fatalf("done notices = %+v, want one to dm-1", done)
	}
}

func TestTasksWithoutAnOriginHaveNoReplyTo(t *testing.T) {
	ctx := context.Background()
	n := &testfakes.Notifier{}
	a := replyApp(t, t.TempDir()+"/haythos.db", n, task.AssistantTurn{Text: "report"})
	id, err := a.CreateTask(ctx, app.Job{Name: "vm-health", Goal: "check", Model: "default"}, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := a.RunTask(ctx, id); err != nil {
		t.Fatalf("run: %v", err)
	}
	done := n.Of(app.NoticeMessage)
	if len(done) != 1 || done[0].ReplyTo != "" {
		t.Fatalf("done notices = %+v, want one with no reply address", done)
	}
}
