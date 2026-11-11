package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
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
		Windows: map[string]int{"default": 1000},
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

func answerOf(tokens int, tag string) string {
	return tag + strings.Repeat("x", tokens*4-2-len(tag))
}

func used(text string, tokens int) task.AssistantTurn {
	return task.AssistantTurn{Text: text, Usage: task.Usage{InputTokens: tokens}}
}

func summaryCalls(r *convRig) (n int) {
	for _, h := range r.model.Histories {
		if len(h) == 2 && h[0].Role == task.RoleSystem && strings.HasPrefix(h[0].Text, "Summarize") {
			n++
		}
	}
	return n
}

func TestCompactionThreshold(t *testing.T) {
	tests := []struct {
		name          string
		turns         []task.AssistantTurn
		sends         int
		wantSummaries int
		wantInLast    []string
		wantNotInLast []string
	}{
		{
			name:       "many short turns below threshold are all replayed",
			turns:      []task.AssistantTurn{used("A1", 100), used("A2", 200), used("A3", 300), used("A4", 400), used("A5", 500)},
			sends:      5,
			wantInLast: []string{"Q1", "Q2", "Q3", "Q4"}, wantNotInLast: []string{"Summary"},
		},
		{
			name:          "anchor at threshold folds all but the tail once",
			turns:         []task.AssistantTurn{used(answerOf(75, "A1"), 100), used(answerOf(75, "A2"), 846), used("SUM", 0), used("A3", 100)},
			sends:         3,
			wantSummaries: 1,
			wantInLast:    []string{"Summary of the earlier conversation (reference only; if it conflicts with the latest user message, the latest message wins):\nSUM", "A2"},
			wantNotInLast: []string{"A1"},
		},
		{
			name:       "anchor just below threshold writes nothing",
			turns:      []task.AssistantTurn{used(answerOf(75, "A1"), 100), used(answerOf(75, "A2"), 845), used("A3", 100)},
			sends:      3,
			wantInLast: []string{"A1", "A2"}, wantNotInLast: []string{"Summary"},
		},
		{
			name:       "no anchor falls back to chars/4 below threshold",
			turns:      []task.AssistantTurn{{Text: answerOf(400, "A1")}, {Text: answerOf(400, "A2")}, {Text: "A3"}},
			sends:      3,
			wantInLast: []string{"A1", "A2"}, wantNotInLast: []string{"Summary"},
		},
		{
			name:          "no anchor falls back to chars/4 at threshold",
			turns:         []task.AssistantTurn{{Text: answerOf(500, "A1")}, {Text: answerOf(500, "A2")}, {Text: "SUM"}, {Text: "A3"}},
			sends:         3,
			wantSummaries: 1,
			wantInLast:    []string{"SUM", "A2"}, wantNotInLast: []string{"A1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newConvRig(t, tc.turns...)
			for i := 1; i <= tc.sends; i++ {
				r.send(t, "c1", false, "Q"+string(rune('0'+i)))
			}
			if got := summaryCalls(r); got != tc.wantSummaries {
				t.Fatalf("summary calls = %d, want %d", got, tc.wantSummaries)
			}
			got := texts(r.model.Histories[len(r.model.Histories)-1])
			for _, w := range tc.wantInLast {
				if !strings.Contains(got, w) {
					t.Errorf("history %.300q missing %q", got, w)
				}
			}
			for _, w := range tc.wantNotInLast {
				if strings.Contains(got, w) {
					t.Errorf("history %.300q must not contain %q", got, w)
				}
			}
		})
	}
}

func TestSummarizerPromptCarriesPreviousSummary(t *testing.T) {
	t.Parallel()
	r := newConvRig(t,
		used(answerOf(75, "A1"), 100), used(answerOf(75, "A2"), 846), used("SUM1", 0), used(answerOf(75, "A3"), 846), used("SUM2", 0), used("A4", 0))
	for _, q := range []string{"Q1", "Q2", "Q3", "Q4"} {
		r.send(t, "c1", false, q)
	}
	var prompts []task.Message
	for _, h := range r.model.Histories {
		if len(h) == 2 && strings.HasPrefix(h[0].Text, "Summarize") {
			prompts = append(prompts, h...)
		}
	}
	if len(prompts) != 4 {
		t.Fatalf("summarizer prompts = %d messages, want 2 summaries", len(prompts))
	}
	for _, want := range []string{"Goal", "Constraints & preferences", "Decisions", "Open questions & pending", "Key facts", "1,500 words"} {
		if !strings.Contains(prompts[0].Text, want) {
			t.Errorf("summary request missing %q", want)
		}
	}
	if strings.Contains(prompts[1].Text, "Earlier summary") {
		t.Errorf("first summary must not reference an earlier one")
	}
	if !strings.Contains(prompts[3].Text, "Earlier summary:\nSUM1") {
		t.Errorf("second summary prompt %.300q must include the previous summary", prompts[3].Text)
	}
}

type summaryFailModel struct{ *testfakes.Model }

func (m summaryFailModel) Step(ctx context.Context, h []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	if len(tools) == 0 && len(h) == 2 && h[0].Role == task.RoleSystem && strings.HasPrefix(h[0].Text, "Summarize") {
		return task.AssistantTurn{}, errors.New("model down")
	}
	return m.Model.Step(ctx, h, tools)
}

func TestFailedSummaryFallsBackToTailAndStillAnswers(t *testing.T) {
	r := newConvRig(t, used(answerOf(75, "A1"), 100), used(answerOf(75, "A2"), 846), used("A3", 0))
	r.app = rebuild(t, r, summaryFailModel{r.model})
	r.send(t, "c1", false, "Q1")
	r.send(t, "c1", false, "Q2")
	r.send(t, "c1", false, "Q3")
	got := texts(r.model.Histories[2])
	if strings.Contains(got, "A1") || !strings.Contains(got, "A2") || strings.Contains(got, "Summary") {
		t.Fatalf("history after failed summary = %.200q, want only the tail", got)
	}
}

type countingFailModel struct {
	*testfakes.Model
	attempts int
}

func (m *countingFailModel) Step(ctx context.Context, h []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	if len(tools) == 0 && len(h) == 2 && h[0].Role == task.RoleSystem && strings.HasPrefix(h[0].Text, "Summarize") {
		m.attempts++
		return task.AssistantTurn{}, errors.New("model down")
	}
	return m.Model.Step(ctx, h, tools)
}

func TestFailedSummaryCannotHideRealSizeBehindSmallAnchor(t *testing.T) {
	r := newConvRig(t, task.AssistantTurn{Text: answerOf(500, "A1")}, task.AssistantTurn{Text: answerOf(500, "A2")}, used("A3", 100), used("A4", 100))
	m := &countingFailModel{Model: r.model}
	r.app = rebuild(t, r, m)
	for _, q := range []string{"Q1", "Q2", "Q3", "Q4"} {
		r.send(t, "c1", false, q)
	}
	if m.attempts != 2 {
		t.Fatalf("summarize attempts = %d, want 2: the small anchor after a failed fold must not suppress the next one", m.attempts)
	}
}

func TestIncomingMessageCountsTowardCompaction(t *testing.T) {
	t.Parallel()
	r := newConvRig(t, used(answerOf(75, "A1"), 100), used(answerOf(75, "A2"), 770), used("SUM", 0), used("A3", 0))
	r.send(t, "c1", false, "Q1")
	r.send(t, "c1", false, "Q2")
	r.send(t, "c1", false, strings.Repeat("q", 320))
	if got := summaryCalls(r); got != 1 {
		t.Fatalf("summary calls = %d, want 1: anchor 845 plus an 80-token message reaches the threshold", got)
	}
}

func TestSingleHugeTurnIsTruncatedInReplay(t *testing.T) {
	t.Parallel()
	r := newConvRig(t, task.AssistantTurn{Text: answerOf(2000, "A1")}, task.AssistantTurn{Text: "A2"})
	r.send(t, "c1", false, "Q1")
	r.send(t, "c1", false, "Q2")
	var replayed string
	for _, m := range r.model.Histories[1] {
		if m.Context && m.Role == task.RoleAssistant {
			replayed = m.Text
		}
	}
	if !strings.HasSuffix(replayed, "[truncated]") || !strings.HasPrefix(replayed, "A1") || len(replayed) > 920*4 {
		t.Fatalf("replayed answer has %d bytes, suffix %q; want a head of the answer that fits the threshold, noted [truncated]", len(replayed), replayed[max(0, len(replayed)-20):])
	}
}

// toolRig is a chat app with one read tool, note.read, whose result is result.
func toolRig(t *testing.T, result task.ToolResult, turns ...task.AssistantTurn) (*app.App, *testfakes.Model, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := testfakes.NewModel(turns...)
	tools := testfakes.NewTools(task.ToolSpec{Name: "note.read", InputSchema: json.RawMessage(`{"type":"object"}`)})
	tools.Handlers["note.read"] = func(task.ToolCall) (task.ToolResult, error) { return result, nil }
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.read"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy:  policy.Policy{Tiers: map[string]policy.Tier{"note.read": policy.TierRead}},
		Clock:   time.Now,
		Windows: map[string]int{"default": 1000},
	})
	return a, model, st
}

func readNote(input int) task.AssistantTurn {
	return task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "note.read", Args: json.RawMessage(`{}`)}}, Usage: task.Usage{InputTokens: input}}
}

func ask(t *testing.T, a *app.App, text string) string {
	t.Helper()
	id, err := a.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: "c1"})
	if err != nil {
		t.Fatalf("handle %q: %v", text, err)
	}
	return id
}

// same compares what a model adapter renders: role, text, calls and results, not the Context mark.
func same(a, b task.Message) bool {
	a.Context, b.Context = false, false
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestFollowUpExtendsTheLastPromptExactly(t *testing.T) {
	t.Parallel()
	a, model, _ := toolRig(t, task.ToolResult{Content: "note body"}, readNote(100), used("A1", 120), used("A2", 150))
	ask(t, a, "Q1")
	ask(t, a, "Q2")
	last, next := model.Histories[1], model.Histories[2]
	want := append(slices.Clone(last), task.Message{Role: task.RoleAssistant, Text: "A1"}, task.Message{Role: task.RoleUser, Text: "Q2"})
	if len(next) != len(want) {
		t.Fatalf("follow-up history has %d messages, want %d:\n%s", len(next), len(want), texts(next))
	}
	for i := range want {
		if !same(next[i], want[i]) {
			t.Fatalf("message %d = %+v, want %+v", i, next[i], want[i])
		}
	}
	for i, m := range next[1 : len(next)-1] {
		if !m.Context {
			t.Errorf("replayed message %d is not marked as context: %+v", i+1, m)
		}
	}
}

func TestReplayedUntrustedToolResultTaintsTheFollowUp(t *testing.T) {
	t.Parallel()
	a, _, _ := toolRig(t, task.ToolResult{Content: "page text", Untrusted: true}, readNote(100), used("A1", 120), used("A2", 150))
	ask(t, a, "Q1")
	id := ask(t, a, "Q2")
	s, err := a.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Tainted {
		t.Error("follow-up task is not tainted although its replay carries an untrusted tool result")
	}
}

func TestAnchorCountsInTaskToolTraffic(t *testing.T) {
	t.Parallel()
	a, model, _ := toolRig(t, task.ToolResult{Content: "note body"}, used(answerOf(75, "A1"), 100), readNote(100), used(answerOf(75, "A2"), 1500), used("SUM", 0), used("A3", 100))
	for _, q := range []string{"Q1", "Q2", "Q3"} {
		ask(t, a, q)
	}
	got := texts(model.Histories[len(model.Histories)-1])
	if !strings.Contains(got, "SUM") || strings.Contains(got, "A1") || !strings.Contains(got, "A2") {
		t.Fatalf("history = %.300q, want A1 folded into the summary: the 1500-token last step of Q2 is replayed now", got)
	}
}

func TestOversizedToolTurnFallsBackToQuestionAndAnswer(t *testing.T) {
	t.Parallel()
	a, model, _ := toolRig(t, task.ToolResult{Content: strings.Repeat("n", 8000)}, readNote(100), used("A1", 120), used("A2", 150))
	ask(t, a, "Q1")
	ask(t, a, "Q2")
	got := texts(model.Histories[2])
	if strings.Contains(got, "nnnn") || !strings.Contains(got, "user:Q1|assistant:A1|user:Q2|") {
		t.Fatalf("history = %.300q, want the 2000-token turn replayed as question and answer only", got)
	}
}

func rebuild(t *testing.T, r *convRig, m app.Model) *app.App {
	t.Helper()
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": m},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default"},
		Tools:      testfakes.NewTools(), Events: r.store, Ledger: r.store, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{}}, Clock: r.clock.Now, ApprovalTTL: time.Hour,
		Windows: map[string]int{"default": 1000},
	})
}

func TestDMSummaryIsDroppedOnceItsTurnsExpire(t *testing.T) {
	r := newConvRig(t,
		used(answerOf(75, "A1"), 100), used(answerOf(75, "A2"), 100), used(answerOf(75, "A3"), 846),
		used("SUMMARY-OF-1-2", 0), used("A4", 100), used("A5", 100))
	r.send(t, "dm", true, "Q1")
	r.clock.Advance(10 * time.Hour)
	r.send(t, "dm", true, "Q2")
	r.clock.Advance(10 * time.Hour)
	r.send(t, "dm", true, "Q3")
	r.send(t, "dm", true, "Q4")
	if h := texts(r.model.Histories[len(r.model.Histories)-1]); !strings.Contains(h, "SUMMARY-OF-1-2") {
		t.Fatalf("history = %.200q, want the summary while its turns are inside 24h", h)
	}
	r.clock.Advance(5 * time.Hour)
	r.send(t, "dm", true, "Q5")
	h := texts(r.model.Histories[len(r.model.Histories)-1])
	if strings.Contains(h, "SUMMARY-OF-1-2") || !strings.Contains(h, "Q3") || !strings.Contains(h, "Q4") {
		t.Fatalf("history = %.300q, want no summary and the surviving turns", h)
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
