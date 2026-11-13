package acceptance_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type delegateRig struct {
	t        *testing.T
	clock    *testfakes.Clock
	notifier *testfakes.Notifier
	store    *sqlite.Store
	app      *app.App
	runtime  *testfakes.Runtime
	state    string
	ran      [][]string
	env      []string
	checkOut string
}

func newDelegateRig(t *testing.T, script func(r *delegateRig) []testfakes.RuntimeStep) *delegateRig {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	guard, err := policy.NewGuard(append(policy.StarterRules(), policy.GuardRule{Name: "no-format", Pattern: `format-disk`, Action: policy.Deny}))
	if err != nil {
		t.Fatal(err)
	}
	r := &delegateRig{
		t: t, clock: testfakes.NewClock(time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)), notifier: &testfakes.Notifier{},
		store: st, state: "before", checkOut: strings.Repeat("x", 20<<10) + "TAIL",
	}
	r.runtime = testfakes.NewRuntime("fake", testfakes.RuntimeScript{Steps: script(r)})
	r.app = app.New(app.Deps{
		Models: map[string]app.Model{}, Events: st, Ledger: st, Notifier: r.notifier, Vault: testfakes.Vault{},
		Clock: r.clock.Now, ApprovalTTL: time.Hour, Runtimes: map[string]app.Runtime{"rt": r.runtime}, Guard: guard,
		WorkdirState: func(context.Context, string) (string, error) { return r.state, nil },
		RunCheck: func(_ context.Context, _ string, argv, env []string, _ time.Duration) (int, string, error) {
			r.ran, r.env = append(r.ran, argv), env
			return 0, r.checkOut, nil
		},
	})
	return r
}

func (r *delegateRig) run(checks ...app.Check) (string, error) {
	return r.app.RunDelegated(context.Background(), app.Job{Name: "delegate", Goal: "g"}, app.Delegation{Runtime: "rt", Prompt: "p", Workdir: "/w", Checks: checks})
}

func askStep(id, kind, text string) testfakes.RuntimeStep {
	return testfakes.Emit(app.RuntimeEvent{Kind: app.RuntimePermissionRequest, Permission: &app.PermissionRequest{
		RequestID: id, Tool: "Bash", Kind: kind, Text: text, AllowOptionID: "allow-" + id, RejectOptionID: "reject-" + id,
	}})
}

func (r *delegateRig) changeWorkdir() testfakes.RuntimeStep {
	return testfakes.Do(func(string, bool) { r.state = "after" })
}

func (r *delegateRig) snap(id string) task.Snapshot {
	r.t.Helper()
	s, err := r.app.Snapshot(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func TestDelegatedAnswersWithTheRuntimesOwnOptionIDs(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"unguarded is allowed", "go test ./...", "allow-q1"},
		{"denied by config is rejected", "format-disk /dev/sda", "reject-q1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDelegateRig(t, func(r *delegateRig) []testfakes.RuntimeStep {
				return []testfakes.RuntimeStep{askStep("q1", "execute", tc.text), r.changeWorkdir()}
			})
			if _, err := r.run(); err != nil {
				t.Fatal(err)
			}
			calls := r.runtime.Session(0).Approvals()
			if len(calls) != 1 || calls[0].OptionID != tc.want {
				t.Errorf("approvals = %+v, want one answer with %s", calls, tc.want)
			}
		})
	}
}

func TestDelegatedUserDecisionUsesTheRuntimesOptionIDs(t *testing.T) {
	tests := []struct {
		name    string
		approve bool
		want    string
	}{
		{"approve", true, "allow-q1"},
		{"reject", false, "reject-q1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newDelegateRig(t, func(r *delegateRig) []testfakes.RuntimeStep {
				return []testfakes.RuntimeStep{askStep("q1", "execute", "rm x"), r.changeWorkdir()}
			})
			done := make(chan error, 1)
			go func() { _, err := r.run(); done <- err }()
			for deadline := time.Now().Add(5 * time.Second); len(r.notifier.Of(app.NoticeApproval)) == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			}
			n := r.notifier.Of(app.NoticeApproval)[0]
			if err := r.app.HandleApproval(context.Background(), n.TaskID, n.Approval.Approval.ID, tc.approve); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			calls := r.runtime.Session(0).Approvals()
			if len(calls) != 1 || calls[0].OptionID != tc.want {
				t.Errorf("approvals = %+v, want one answer with %s", calls, tc.want)
			}
		})
	}
}

func TestDelegatedApprovalExpiryRejectsThroughTick(t *testing.T) {
	r := newDelegateRig(t, func(r *delegateRig) []testfakes.RuntimeStep {
		return []testfakes.RuntimeStep{askStep("q1", "execute", "rm x"), r.changeWorkdir()}
	})
	done := make(chan error, 1)
	go func() { _, err := r.run(); done <- err }()
	for deadline := time.Now().Add(5 * time.Second); len(r.notifier.Of(app.NoticeApproval)) == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	r.clock.Advance(2 * time.Hour)
	if err := r.app.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	calls := r.runtime.Session(0).Approvals()
	if len(calls) != 1 || calls[0].OptionID != "reject-q1" {
		t.Errorf("approvals = %+v, want the request rejected after expiry", calls)
	}
}

func TestDelegatedChecks(t *testing.T) {
	r := newDelegateRig(t, func(r *delegateRig) []testfakes.RuntimeStep { return []testfakes.RuntimeStep{r.changeWorkdir()} })
	id, err := r.run(app.Check{Name: "go-test", Argv: []string{"go", "test", "./..."}, Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if s := r.snap(id); s.State != task.Done {
		t.Fatalf("state = %s (%s), want done", s.State, s.Reason)
	}
	if len(r.ran) != 1 || strings.Join(r.ran[0], " ") != "go test ./..." {
		t.Errorf("ran = %v, want go test ./...", r.ran)
	}
	if len(r.env) != 1 || r.env[0] != "HAYTHOS_TASK="+id {
		t.Errorf("env = %v, want only HAYTHOS_TASK", r.env)
	}
	evs, err := r.store.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == task.EvCheckRun {
			if len(e.Payload) > 9<<10 || !strings.Contains(string(e.Payload), "TAIL") {
				t.Errorf("check event is %d bytes, want the capped tail of the output", len(e.Payload))
			}
		}
	}
}

func TestDelegatedGuards(t *testing.T) {
	t.Run("unknown runtime", func(t *testing.T) {
		r := newDelegateRig(t, func(*delegateRig) []testfakes.RuntimeStep { return nil })
		_, err := r.app.RunDelegated(context.Background(), app.Job{Name: "d"}, app.Delegation{Runtime: "nope"})
		if err == nil {
			t.Error("RunDelegated with an unknown runtime succeeded")
		}
	})
	t.Run("the model loop refuses a delegated task", func(t *testing.T) {
		r := newDelegateRig(t, func(r *delegateRig) []testfakes.RuntimeStep { return []testfakes.RuntimeStep{testfakes.Hang()} })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan string, 1)
		go func() {
			id, _ := r.app.RunDelegated(ctx, app.Job{Name: "d", Goal: "g"}, app.Delegation{Runtime: "rt", Workdir: "/w"})
			done <- id
		}()
		var id string
		for deadline := time.Now().Add(5 * time.Second); id == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			evs, _ := r.store.Since(context.Background(), 0)
			for _, e := range evs {
				if e.Type == task.EvCreated {
					id = e.TaskID
				}
			}
		}
		for deadline := time.Now().Add(5 * time.Second); r.runtime.Sessions() == 0 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		}
		if err := r.app.RunTask(context.Background(), id); !errors.Is(err, app.ErrDelegated) {
			t.Errorf("RunTask error = %v, want ErrDelegated", err)
		}
		cancel()
		<-done
	})
}

func TestDelegatedCancelRacingApprovalKeepsTheLogReplayable(t *testing.T) {
	for i := range 100 {
		r := newDelegateRig(t, func(r *delegateRig) []testfakes.RuntimeStep {
			return []testfakes.RuntimeStep{askStep("q1", "execute", "rm x"), r.changeWorkdir()}
		})
		type result struct {
			id  string
			err error
		}
		done := make(chan result, 1)
		go func() { id, err := r.run(); done <- result{id, err} }()
		for deadline := time.Now().Add(5 * time.Second); len(r.notifier.Of(app.NoticeApproval)) == 0 && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		}
		n := r.notifier.Of(app.NoticeApproval)[0]
		go func() { _ = r.app.HandleApproval(context.Background(), n.TaskID, n.Approval.Approval.ID, true) }()
		_ = r.app.CancelDelegated(context.Background(), n.TaskID)
		res := <-done
		if res.err != nil {
			t.Fatalf("iteration %d: run: %v", i, res.err)
		}
		if _, err := r.app.Snapshot(context.Background(), res.id); err != nil {
			t.Fatalf("iteration %d: replay: %v", i, err)
		}
	}
}
