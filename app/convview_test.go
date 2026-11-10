package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func viewApp(t *testing.T, clock *testfakes.Clock, n *testfakes.Notifier, turns ...task.AssistantTurn) *app.App {
	t.Helper()
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.add"}},
		Tools:      testfakes.NewTools(task.ToolSpec{Name: "note.add", Write: true, TargetArg: "id", InputSchema: json.RawMessage(`{"type":"object"}`)}),
		Events:     &testfakes.Events{}, Ledger: testfakes.NewLedger(), Notifier: n, Vault: testfakes.Vault{},
		Policy:      policy.Policy{Tiers: map[string]policy.Tier{"note.add": policy.TierInternalWrite}},
		Clock:       clock.Now,
		ApprovalTTL: time.Hour,
	})
}

func writeTurn(id string) task.AssistantTurn {
	return task.AssistantTurn{Calls: []task.ToolCall{{ID: "c-" + id, Name: "note.add", Args: json.RawMessage(`{"id":"` + id + `"}`)}}}
}

func say(t *testing.T, a *app.App, clock *testfakes.Clock, replyTo, text string) string {
	t.Helper()
	clock.Advance(time.Minute)
	id, err := a.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: replyTo})
	if err != nil {
		t.Fatalf("handle input %q: %v", text, err)
	}
	return id
}

func TestConversationViewTurnsAndPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := testfakes.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	n := &testfakes.Notifier{}
	a := viewApp(t, clock, n,
		task.AssistantTurn{Text: "hello"},
		task.AssistantTurn{Text: "fine"},
		writeTurn("n1"),
		writeTurn("n2"),
	)
	say(t, a, clock, "tui:a", "hi")
	say(t, a, clock, "tui:a", "how are you")
	waitingA := say(t, a, clock, "tui:a", "add a note")
	say(t, a, clock, "tui:b", "add another note")

	v, err := a.ConversationView(ctx, "tui:a")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if len(v.Turns) != 2 || v.Turns[0].User != "hi" || v.Turns[0].Answer != "hello" || v.Turns[1].User != "how are you" {
		t.Fatalf("turns = %+v, want hi/hello then how are you/fine", v.Turns)
	}
	if v.Turns[0].At.IsZero() {
		t.Fatalf("turn time is zero")
	}
	asks := n.Of(app.NoticeApproval)
	var wantID string
	for _, ask := range asks {
		if ask.TaskID == waitingA {
			wantID = ask.Approval.Approval.ID
		}
	}
	if len(v.Pending) != 1 || v.Pending[0].TaskID != waitingA || v.Pending[0].Approval.Approval.ID != wantID || wantID == "" {
		t.Fatalf("pending = %+v, want one for task %s approval %q", v.Pending, waitingA, wantID)
	}
	if v.Pending[0].Approval.Approval.Tool != "note.add" || v.Pending[0].Approval.Tier != policy.TierInternalWrite {
		t.Fatalf("pending approval = %+v, want note.add at tier 1", v.Pending[0].Approval)
	}

	if err := a.HandleApproval(ctx, waitingA, wantID, false); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if v, err = a.ConversationView(ctx, "tui:a"); err != nil || len(v.Pending) != 0 {
		t.Fatalf("pending after decide = %+v, %v; want none", v.Pending, err)
	}
}

func TestConversationsByPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := testfakes.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	long := strings.Repeat("é", 70)
	a := viewApp(t, clock, &testfakes.Notifier{},
		task.AssistantTurn{Text: "one"},
		task.AssistantTurn{Text: "two"},
		task.AssistantTurn{Text: "three"},
		task.AssistantTurn{Text: "four"},
	)
	say(t, a, clock, "tui:a", long)
	say(t, a, clock, "tui:b", "first on b")
	say(t, a, clock, "123", "discord")
	say(t, a, clock, "tui:a", "second on a")

	got, err := a.Conversations(ctx, "tui:")
	if err != nil {
		t.Fatalf("conversations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("conversations = %+v, want 2", got)
	}
	if got[0].ReplyTo != "tui:a" || got[1].ReplyTo != "tui:b" {
		t.Fatalf("order = %s, %s; want tui:a then tui:b (newest first)", got[0].ReplyTo, got[1].ReplyTo)
	}
	if want := strings.Repeat("é", 60); got[0].FirstUser != want {
		t.Fatalf("first user = %q, want 60 runes of the first message", got[0].FirstUser)
	}
	if got[1].FirstUser != "first on b" {
		t.Fatalf("first user of b = %q", got[1].FirstUser)
	}
	if !got[0].LastAt.After(got[1].LastAt) {
		t.Fatalf("last at a=%s b=%s, want a newer", got[0].LastAt, got[1].LastAt)
	}
}

func TestConversationsIncludeOneWaitingOnItsFirstMessage(t *testing.T) {
	t.Parallel()
	clock := testfakes.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	a := viewApp(t, clock, &testfakes.Notifier{}, writeTurn("n1"))
	say(t, a, clock, "tui:new", "add a note")

	got, err := a.Conversations(context.Background(), "tui:")
	if err != nil {
		t.Fatalf("conversations: %v", err)
	}
	if len(got) != 1 || got[0].ReplyTo != "tui:new" || got[0].FirstUser != "add a note" {
		t.Fatalf("conversations = %+v, want tui:new with its first message", got)
	}
}

func TestTaskOrigin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := testfakes.NewClock(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC))
	a := viewApp(t, clock, &testfakes.Notifier{}, task.AssistantTurn{Text: "hi"})
	id := say(t, a, clock, "tui:x", "hello")

	got, err := a.TaskOrigin(ctx, id)
	if err != nil || got != "tui:x" {
		t.Fatalf("origin = %q, %v; want tui:x", got, err)
	}
	if _, err := a.TaskOrigin(ctx, "chat-missing"); err == nil {
		t.Fatalf("origin of a missing task: want an error")
	}
}
