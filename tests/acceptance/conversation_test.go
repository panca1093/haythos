package acceptance_test

import (
	"context"
	"encoding/json"
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

const m2bToolOutput = "disk 40% SECRET-TOOL-OUTPUT"

// m2bModel answers every chat step with reply(history) and every tool-less call (a summary request) with "SUMMARY-n".
type m2bModel struct {
	mu        sync.Mutex
	reply     func(history []task.Message) task.AssistantTurn
	histories [][]task.Message
	summaries int
}

func (m *m2bModel) Step(_ context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(tools) == 0 {
		m.summaries++
		return task.AssistantTurn{Text: "SUMMARY-" + string(rune('0'+m.summaries))}, nil
	}
	m.histories = append(m.histories, history)
	return m.reply(history), nil
}

func (m *m2bModel) last() []task.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.histories[len(m.histories)-1]
}

type m2bRig struct {
	t     *testing.T
	db    string
	clock *testfakes.Clock
	model *m2bModel
	app   *app.App
}

var m2bJob = app.Job{
	Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"vm.read"},
	Budget: task.Budget{Tokens: 1000000, Steps: 20},
}

func newM2bRig(t *testing.T) *m2bRig {
	t.Helper()
	r := &m2bRig{t: t, db: t.TempDir() + "/haythos.db", clock: testfakes.NewClock(m2Start)}
	r.boot()
	return r
}

// boot builds a fresh process on the same database. A chat step that has not yet seen a tool result calls vm.read once, then answers "answer to <last user text>" (or bigAnswer for texts starting "big").
func (r *m2bRig) boot() {
	r.t.Helper()
	st, err := sqlite.Open(r.db)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.t.Cleanup(func() { _ = st.Close() })
	r.model = &m2bModel{reply: func(h []task.Message) task.AssistantTurn {
		last := h[len(h)-1]
		if last.Role == task.RoleUser {
			return task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "vm.read", Args: json.RawMessage(`{}`)}}}
		}
		var user string
		for _, m := range h {
			if m.Role == task.RoleUser && !strings.HasPrefix(m.Text, "Run the scheduled job") {
				user = m.Text
			}
		}
		if strings.HasPrefix(user, "big") {
			return task.AssistantTurn{Text: "answer to " + user + " " + strings.Repeat("x", 12000)}
		}
		return task.AssistantTurn{Text: "answer to " + user}
	}}
	tools := testfakes.NewTools(task.ToolSpec{Name: "vm.read", InputSchema: json.RawMessage(`{"type":"object"}`)})
	tools.Handlers["vm.read"] = func(task.ToolCall) (task.ToolResult, error) {
		return task.ToolResult{Content: m2bToolOutput}, nil
	}
	r.app = app.New(app.Deps{
		Models: map[string]app.Model{"default": r.model}, DefaultJob: m2bJob,
		Jobs:  []app.Job{{Name: "vm-health", Goal: "Report VM health.", Model: "default", Tools: []string{"vm.read"}, Trigger: app.Trigger{Manual: true}}},
		Tools: tools, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Version: "v1", Tiers: map[string]policy.Tier{"vm.read": policy.TierRead}},
		Clock:  r.clock.Now, ApprovalTTL: time.Hour,
		Windows: map[string]int{"default": 70000}, CompactAt: 0.12,
	})
	if err := r.app.Start(context.Background()); err != nil {
		r.t.Fatalf("start: %v", err)
	}
}

// send runs one chat message and returns the history the model saw on its first step.
func (r *m2bRig) send(replyTo string, dm bool, text string) []task.Message {
	r.t.Helper()
	before := len(r.model.histories)
	if _, err := r.app.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: replyTo, DM: dm}); err != nil {
		r.t.Fatalf("handle input %q: %v", text, err)
	}
	if len(r.model.histories) == before {
		r.t.Fatalf("model never stepped for %q", text)
	}
	return r.model.histories[before]
}

func m2bSees(t *testing.T, h []task.Message, want ...string) {
	t.Helper()
	flat := m2bFlat(h)
	for _, w := range want {
		if !strings.Contains(flat, w) {
			t.Fatalf("history is missing %q:\n%s", w, flat)
		}
	}
}

func m2bLacks(t *testing.T, h []task.Message, nope ...string) {
	t.Helper()
	flat := m2bFlat(h)
	for _, n := range nope {
		if strings.Contains(flat, n) {
			t.Fatalf("history must not contain %q:\n%s", n, flat)
		}
	}
}

// M2B-1. A later message to the same reply_to sees the earlier turn whole: user text, tool calls and results, and answer (amended 2026-11-11 with the owner's approval, so the prompt cache carries across messages).
func TestSecondMessageSeesFirstTurnWithToolTraffic(t *testing.T) {
	r := newM2bRig(t)
	r.send("thread-1", false, "first question")
	h := r.send("thread-1", false, "second question")
	m2bSees(t, h, "user: first question\nassistant: \ncall vm.read\ntool: \nresult "+m2bToolOutput+"\nassistant: answer to first question\nuser: second question")
}

// M2B-2. A different reply_to sees nothing of the first conversation.
func TestOtherConversationSeesNothing(t *testing.T) {
	r := newM2bRig(t)
	r.send("thread-1", false, "first question")
	m2bSees(t, r.send("thread-1", false, "again"), "first question")
	m2bLacks(t, r.send("thread-2", false, "elsewhere"), "first question", "answer to first question")
}

// M2B-3. A DM turn older than 24 h is dropped; a thread turn of the same age is kept.
func TestDMLooksBack24HoursThreadsDoNot(t *testing.T) {
	r := newM2bRig(t)
	r.send("dm-1", true, "dm old")
	r.send("thread-1", false, "thread old")
	r.clock.Advance(time.Hour)
	m2bSees(t, r.send("dm-1", true, "dm soon"), "dm old")
	r.clock.Advance(26 * time.Hour)
	m2bLacks(t, r.send("dm-1", true, "dm later"), "dm old", "dm soon")
	m2bSees(t, r.send("thread-1", false, "thread later"), "thread old")
}

// M2B-4. Past CompactAt of the model window (8,400 tokens in this rig) the oldest turns become a summary that survives a restart and is not recomputed.
func TestOverflowFoldsIntoSummaryThatSurvivesRestart(t *testing.T) {
	r := newM2bRig(t)
	for _, q := range []string{"big one", "big two", "big three"} {
		r.send("thread-1", false, q)
	}
	if r.model.summaries != 0 {
		t.Fatalf("summaries = %d before overflow was asked about, want 0", r.model.summaries)
	}
	h := r.send("thread-1", false, "small four")
	if r.model.summaries != 1 {
		t.Fatalf("summaries = %d, want exactly 1", r.model.summaries)
	}
	m2bSees(t, h, "SUMMARY-1", "big two", "big three", "small four")
	m2bLacks(t, h, "answer to big one")
	if i, j := strings.Index(m2bFlat(h), "SUMMARY-1"), strings.Index(m2bFlat(h), "big two"); i > j {
		t.Fatalf("summary must come before the verbatim turns:\n%s", m2bFlat(h))
	}

	r.boot()
	h = r.send("thread-1", false, "small five")
	if r.model.summaries != 0 {
		t.Fatalf("after restart summaries = %d, want 0 (same turns, already folded)", r.model.summaries)
	}
	m2bSees(t, h, "SUMMARY-1", "big two", "big three", "small four", "small five")
	m2bLacks(t, h, "answer to big one")
}

// M2B-5. Tasks without reply_to get no conversation history.
func TestScheduledTasksGetNoHistory(t *testing.T) {
	r := newM2bRig(t)
	r.send("thread-1", false, "first question")
	m2bSees(t, r.send("thread-1", false, "again"), "first question")

	if _, err := r.app.RunJob(context.Background(), "vm-health"); err != nil {
		t.Fatalf("run job: %v", err)
	}
	h := r.model.last()
	m2bLacks(t, h, "first question", "answer to")
	if len(h) < 2 || h[0].Role != task.RoleSystem || h[1].Role != task.RoleUser || h[1].Context {
		t.Fatalf("scheduled history = %+v, want the system goal and the run instruction first, no conversation context", h)
	}
	m2bLacks(t, r.send("", false, "no origin"), "first question", "answer to")
}
