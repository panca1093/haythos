package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
	"github.com/panca/haythos/internal/testfakes"
)

type convRig struct {
	app   *app.App
	model *testfakes.Model
	clock *testfakes.Clock
	store *sqlite.Store
}

func newConvRig(t *testing.T, turns ...task.AssistantTurn) *convRig {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &convRig{model: testfakes.NewModel(turns...), clock: testfakes.NewClock(time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)), store: st}
	r.app = app.New(app.Deps{
		Models:     map[string]app.Model{"default": r.model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default"},
		Tools:      testfakes.NewTools(), Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{}}, Clock: r.clock.Now, ApprovalTTL: time.Hour,
	})
	return r
}

func (r *convRig) send(t *testing.T, replyTo string, dm bool, text string) {
	t.Helper()
	if _, err := r.app.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: replyTo, DM: dm}); err != nil {
		t.Fatalf("handle input %q: %v", text, err)
	}
}

func texts(h []task.Message) string {
	var b strings.Builder
	for _, m := range h {
		b.WriteString(string(m.Role) + ":" + m.Text + "|")
	}
	return b.String()
}

func TestConversationHistoryRules(t *testing.T) {
	tests := []struct {
		name      string
		dm        bool
		advance   time.Duration
		wantFirst bool
	}{
		{"thread keeps a turn older than 24h", false, 48 * time.Hour, true},
		{"dm keeps a turn just inside 24h", true, 24*time.Hour - time.Second, true},
		{"dm keeps a turn exactly 24h old", true, 24 * time.Hour, true},
		{"dm drops a turn past 24h", true, 24*time.Hour + time.Second, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newConvRig(t, task.AssistantTurn{Text: "A1"}, task.AssistantTurn{Text: "A2"})
			r.send(t, "c1", tc.dm, "Q1")
			r.clock.Advance(tc.advance)
			r.send(t, "c1", tc.dm, "Q2")
			got := texts(r.model.Histories[1])
			if has := strings.Contains(got, "user:Q1|assistant:A1"); has != tc.wantFirst {
				t.Fatalf("history %q: first turn present = %v, want %v", got, has, tc.wantFirst)
			}
		})
	}
}

// answerOf pads so that "Q1" + the answer is exactly tokens*4 bytes.
func answerOf(tokens int, tag string) string {
	return tag + strings.Repeat("x", tokens*4-2-len(tag))
}

func TestConversationWindowBoundary(t *testing.T) {
	tests := []struct {
		name          string
		turnTokens    int
		wantSummaries int
	}{
		{"two turns exactly at 7000 tokens stay verbatim", 3500, 0},
		{"one token over folds the oldest", 3501, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			turns := []task.AssistantTurn{{Text: answerOf(tc.turnTokens, "A1")}, {Text: answerOf(tc.turnTokens, "A2")}}
			if tc.wantSummaries > 0 {
				turns = append(turns, task.AssistantTurn{Text: "SUM"})
			}
			turns = append(turns, task.AssistantTurn{Text: "A3"})
			r := newConvRig(t, turns...)
			r.send(t, "c1", false, "Q1")
			r.send(t, "c1", false, "Q1")
			r.send(t, "c1", false, "Q3")
			calls := len(r.model.Histories)
			if want := 3 + tc.wantSummaries; calls != want {
				t.Fatalf("model calls = %d, want %d", calls, want)
			}
		})
	}
}

type summaryFailModel struct{ *testfakes.Model }

func (m summaryFailModel) Step(ctx context.Context, h []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	if len(tools) == 0 && len(h) == 2 && h[0].Role == task.RoleSystem && strings.HasPrefix(h[0].Text, "Summarize") {
		return task.AssistantTurn{}, errors.New("model down")
	}
	return m.Model.Step(ctx, h, tools)
}

func TestFailedSummaryDropsFoldedTurnsAndStillAnswers(t *testing.T) {
	r := newConvRig(t, task.AssistantTurn{Text: answerOf(3501, "A1")}, task.AssistantTurn{Text: answerOf(3501, "A2")}, task.AssistantTurn{Text: "A3"}, task.AssistantTurn{Text: "A4"})
	r.app = rebuild(t, r, summaryFailModel{r.model})
	r.send(t, "c1", false, "Q1")
	r.send(t, "c1", false, "Q2")
	r.send(t, "c1", false, "Q3")
	got := texts(r.model.Histories[2])
	if strings.Contains(got, "A1") || !strings.Contains(got, "A2") || strings.Contains(got, "Summary") {
		t.Fatalf("history after failed summary = %.200q, want only the second turn", got)
	}
}

func rebuild(t *testing.T, r *convRig, m app.Model) *app.App {
	t.Helper()
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": m},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default"},
		Tools:      testfakes.NewTools(), Events: r.store, Ledger: r.store, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{}}, Clock: r.clock.Now, ApprovalTTL: time.Hour,
	})
}

func TestDMSummaryIsDroppedOnceItsTurnsExpire(t *testing.T) {
	r := newConvRig(t,
		task.AssistantTurn{Text: answerOf(3000, "A1")}, task.AssistantTurn{Text: answerOf(3000, "A2")}, task.AssistantTurn{Text: answerOf(3000, "A3")},
		task.AssistantTurn{Text: "SUMMARY-OF-1"}, task.AssistantTurn{Text: "A4"}, task.AssistantTurn{Text: "A5"})
	r.send(t, "dm", true, "Q1")
	r.clock.Advance(10 * time.Hour)
	r.send(t, "dm", true, "Q2")
	r.clock.Advance(10 * time.Hour)
	r.send(t, "dm", true, "Q3")
	r.send(t, "dm", true, "Q4")
	if h := texts(r.model.Histories[len(r.model.Histories)-1]); !strings.Contains(h, "SUMMARY-OF-1") {
		t.Fatalf("history = %.200q, want the summary while its turn is inside 24h", h)
	}
	r.clock.Advance(5 * time.Hour)
	r.send(t, "dm", true, "Q5")
	h := texts(r.model.Histories[len(r.model.Histories)-1])
	if strings.Contains(h, "SUMMARY-OF-1") || !strings.Contains(h, "Q2") || !strings.Contains(h, "Q3") {
		t.Fatalf("history = %.300q, want no summary and the two newest turns", h)
	}
}

func TestDuplicateTurnRecordIsCountedOnce(t *testing.T) {
	ctx := context.Background()
	r := newConvRig(t, task.AssistantTurn{Text: "A1"}, task.AssistantTurn{Text: "A2"})
	r.send(t, "c1", false, "Q1")
	events, err := r.store.Load(ctx, "conv:c1")
	if err != nil || len(events) != 1 {
		t.Fatalf("conversation events = %d, %v; want 1", len(events), err)
	}
	if _, err := r.store.Append(ctx, "conv:c1", events[0].Type, events[0].Payload); err != nil {
		t.Fatalf("append duplicate: %v", err)
	}
	r.send(t, "c1", false, "Q2")
	if got := strings.Count(texts(r.model.Histories[1]), "user:Q1|"); got != 1 {
		t.Fatalf("first turn appears %d times, want 1", got)
	}
}
