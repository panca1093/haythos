package app_test

import (
	"context"
	"errors"
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

// flakyExtractModel answers chat with "ok" and fails the first failures extraction steps, as when the machine is offline.
type flakyExtractModel struct {
	mu       sync.Mutex
	failures int
	attempts int
}

func (m *flakyExtractModel) Step(_ context.Context, _ []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.ContainsFunc(tools, func(t task.ToolSpec) bool { return t.Name == "brain.remember" }) {
		return task.AssistantTurn{Text: "ok"}, nil
	}
	m.attempts++
	if m.attempts <= m.failures {
		return task.AssistantTurn{}, errors.New("can't reach the API server")
	}
	return task.AssistantTurn{Text: "done"}, nil
}

func (m *flakyExtractModel) extractAttempts() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempts
}

func newExtractRig(t *testing.T, model *flakyExtractModel, clock *testfakes.Clock) *app.App {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	brain := testfakes.NewBrain()
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Budget: task.Budget{Tokens: 1000000, Steps: 20}},
		Tools:      testfakes.NewBrainTools(brain), Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{}, Brain: brain,
		Policy: policy.Policy{Version: "v1", Tiers: map[string]policy.Tier{
			"brain.recall": policy.TierRead, "brain.remember": policy.TierRead,
		}},
		Clock: clock.Now, ApprovalTTL: time.Hour,
	})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	return a
}

func TestFailedExtractionIsRetriedLaterNotLost(t *testing.T) {
	ctx := context.Background()
	clock := testfakes.NewClock(time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC))
	model := &flakyExtractModel{failures: 1}
	a := newExtractRig(t, model, clock)
	tick := func() {
		t.Helper()
		if err := a.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}

	if _, err := a.HandleInput(ctx, app.Input{Text: "we decided to use sqlite", ReplyTo: "thread-1"}); err != nil {
		t.Fatalf("handle input: %v", err)
	}
	clock.Advance(3 * time.Hour)
	tick()
	if got := model.extractAttempts(); got != 1 {
		t.Fatalf("attempts after going stale = %d, want 1 (failed)", got)
	}

	clock.Advance(time.Minute)
	tick()
	if got := model.extractAttempts(); got != 1 {
		t.Fatalf("attempts one minute after the failure = %d, want still 1 (retry waits)", got)
	}

	clock.Advance(time.Hour)
	tick()
	if got := model.extractAttempts(); got != 2 {
		t.Fatalf("attempts an hour after the failure = %d, want 2 (retried)", got)
	}

	clock.Advance(time.Hour)
	tick()
	if got := model.extractAttempts(); got != 2 {
		t.Fatalf("attempts after a successful retry = %d, want still 2", got)
	}
}

func TestExtractionGivesUpAfterRepeatedFailures(t *testing.T) {
	ctx := context.Background()
	clock := testfakes.NewClock(time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC))
	model := &flakyExtractModel{failures: 100}
	a := newExtractRig(t, model, clock)

	if _, err := a.HandleInput(ctx, app.Input{Text: "we decided to use sqlite", ReplyTo: "thread-1"}); err != nil {
		t.Fatalf("handle input: %v", err)
	}
	clock.Advance(3 * time.Hour)
	for range 10 {
		if err := a.Tick(ctx); err != nil {
			t.Fatalf("tick: %v", err)
		}
		clock.Advance(time.Hour)
	}
	if got := model.extractAttempts(); got != 3 {
		t.Fatalf("attempts = %d, want 3 then give up", got)
	}
}
