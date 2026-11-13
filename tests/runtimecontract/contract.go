// Package runtimecontract is the behaviour every app.Runtime implementation must show to the kernel.
//
// An adapter's test calls RunRuntimeContract with a factory that builds its runtime so that the first Send plays the
// given Script (a scripted helper process, not the real tool). The tests run app.RunDelegated against it on a real
// sqlite store. Frozen: if a case is wrong, leave it failing and say so; do not edit it to pass.
package runtimecontract

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

type StepKind int

const (
	StepSay StepKind = iota
	StepAsk
	StepWrite
	StepCrash
	StepHang
)

// Step is one scripted action of a runtime turn.
type Step struct {
	Kind StepKind
	// Chunks of one agent message (StepSay), all with the same message id.
	Chunks []string
	// ReqKind and Text describe the permission request (StepAsk): an ACP tool kind and the command or path.
	ReqKind, Text string
	// Path and Content are written under the workdir (StepWrite).
	Path, Content string
}

// Say streams one agent message in chunks.
func Say(chunks ...string) Step { return Step{Kind: StepSay, Chunks: chunks} }

// Ask sends a permission request and blocks the turn until it is answered.
func Ask(reqKind, text string) Step { return Step{Kind: StepAsk, ReqKind: reqKind, Text: text} }

// Write creates a file in the workdir, unless a request of the turn was rejected.
func Write(path, content string) Step { return Step{Kind: StepWrite, Path: path, Content: content} }

// Crash kills the session mid-turn: Events closes and Send fails.
func Crash() Step { return Step{Kind: StepCrash} }

// Hang blocks the turn until Stop.
func Hang() Step { return Step{Kind: StepHang} }

// Script is what the runtime does on its first Send, then it ends the turn with end_turn.
type Script struct {
	Steps []Step
	// RejectEndsTurn: a rejected request ends the turn as cancelled (opencode); otherwise the turn goes on (claude, codex).
	RejectEndsTurn bool
}

// NewRuntime builds a runtime whose first Send plays the script, in whatever workdir RuntimeStart names.
type NewRuntime func(t *testing.T, s Script) app.Runtime

var errCrash = errors.New("simulated kernel crash")

type rig struct {
	t        *testing.T
	db       string
	workdir  string
	clock    *testfakes.Clock
	notifier *testfakes.Notifier
	store    *sqlite.Store
	app      *app.App
	hook     func(point string) error
}

func newRig(t *testing.T, rt app.Runtime) *rig {
	t.Helper()
	r := &rig{
		t: t, db: t.TempDir() + "/haythos.db", workdir: t.TempDir(),
		clock:    testfakes.NewClock(time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)),
		notifier: &testfakes.Notifier{},
	}
	if err := os.WriteFile(filepath.Join(r.workdir, "victim.txt"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.boot(rt)
	return r
}

func (r *rig) boot(rt app.Runtime) {
	r.t.Helper()
	if r.store != nil {
		_ = r.store.Close()
	}
	st, err := sqlite.Open(r.db)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.t.Cleanup(func() { _ = st.Close() })
	r.store = st
	guard, err := policy.NewGuard(append(policy.StarterRules(), policy.GuardRule{
		Name: "no-disk-tools", Kinds: []string{policy.KindExecute}, Pattern: `\bformat-disk\b`, Action: policy.Deny,
	}))
	if err != nil {
		r.t.Fatal(err)
	}
	r.app = app.New(app.Deps{
		Models: map[string]app.Model{}, Events: st, Ledger: st, Notifier: r.notifier, Vault: testfakes.Vault{},
		Clock: r.clock.Now, ApprovalTTL: time.Hour, Runtimes: map[string]app.Runtime{"rt": rt}, Guard: guard,
		WorkdirState: fingerprint,
		RunCheck: func(_ context.Context, _ string, argv, _ []string, _ time.Duration) (int, string, error) {
			if argv[0] == "fail" {
				return 1, "FAIL: TestX", nil
			}
			return 0, "ok", nil
		},
		Hook: func(p string) error {
			if r.hook != nil {
				return r.hook(p)
			}
			return nil
		},
	})
}

func fingerprint(_ context.Context, dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("open workdir: %w", err)
	}
	defer func() { _ = root.Close() }()
	h := sha256.New()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root.FS(), p)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s", p, len(b), b)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil)), err
}

type outcome struct {
	id  string
	err error
}

func (r *rig) run(ctx context.Context, checks ...app.Check) <-chan outcome {
	out := make(chan outcome, 1)
	go func() {
		id, err := r.app.RunDelegated(ctx, app.Job{Name: "delegate", Goal: "Delegated work."}, app.Delegation{
			Runtime: "rt", Prompt: "do the thing", Workdir: r.workdir, Checks: checks,
		})
		out <- outcome{id, err}
	}()
	return out
}

func (r *rig) wait(out <-chan outcome) outcome {
	r.t.Helper()
	select {
	case o := <-out:
		return o
	case <-time.After(10 * time.Second):
		r.t.Fatal("RunDelegated did not return")
		return outcome{}
	}
}

func (r *rig) eventually(what string, cond func() bool) {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	r.t.Fatalf("timed out waiting for %s", what)
}

func (r *rig) taskID() string {
	r.t.Helper()
	var id string
	r.eventually("task creation", func() bool {
		evs, err := r.store.Since(context.Background(), 0)
		if err != nil {
			r.t.Fatal(err)
		}
		for _, e := range evs {
			if e.Type == task.EvCreated {
				id = e.TaskID
				return true
			}
		}
		return false
	})
	return id
}

func (r *rig) events(id string) []task.Event {
	r.t.Helper()
	evs, err := r.store.Load(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return evs
}

func (r *rig) snap(id string) task.Snapshot {
	r.t.Helper()
	s, err := r.app.Snapshot(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return s
}

func (r *rig) awaitApproval() (taskID, approvalID string) {
	r.t.Helper()
	r.eventually("approval notice", func() bool { return len(r.notifier.Of(app.NoticeApproval)) > 0 })
	n := r.notifier.Of(app.NoticeApproval)[0]
	return n.TaskID, n.Approval.Approval.ID
}

func (r *rig) outExists() bool {
	_, err := os.Stat(filepath.Join(r.workdir, "out.txt"))
	return err == nil
}

type loggedEvent struct {
	Kind app.RuntimeEventKind `json:"kind"`
	Text string               `json:"text"`
}

// firstIndex is the position of the first event of type typ whose payload satisfies match, or -1.
func firstIndex(evs []task.Event, typ task.EventType, match func(json.RawMessage) bool) int {
	return slices.IndexFunc(evs, func(e task.Event) bool { return e.Type == typ && match(e.Payload) })
}

func any_(json.RawMessage) bool { return true }

func hasState(s task.State) func(json.RawMessage) bool {
	return func(p json.RawMessage) bool {
		var c task.StateChange
		return json.Unmarshal(p, &c) == nil && c.State == s
	}
}

func hasKind(k app.RuntimeEventKind) func(json.RawMessage) bool {
	return func(p json.RawMessage) bool {
		var e loggedEvent
		return json.Unmarshal(p, &e) == nil && e.Kind == k
	}
}

func verdictIs(action policy.Action) func(json.RawMessage) bool {
	return func(p json.RawMessage) bool {
		var v struct {
			Action policy.Action `json:"action"`
		}
		return json.Unmarshal(p, &v) == nil && v.Action == action
	}
}

// RunRuntimeContract runs every case against runtimes built by newRuntime.
func RunRuntimeContract(t *testing.T, newRuntime NewRuntime) {
	t.Run("an unguarded command is answered at once", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Ask("execute", "go test ./..."), Write("out.txt", "x")}}))
		o := r.wait(r.run(context.Background()))
		if o.err != nil {
			t.Fatal(o.err)
		}
		if s := r.snap(o.id); s.State != task.Done {
			t.Errorf("state = %s (%s), want done", s.State, s.Reason)
		}
		if n := len(r.notifier.Of(app.NoticeApproval)); n != 0 {
			t.Errorf("approval notices = %d, want 0", n)
		}
		if !r.outExists() {
			t.Error("out.txt missing: the runtime was not answered with allow")
		}
		evs := r.events(o.id)
		ask, verdict := firstIndex(evs, task.EvRuntimeEvent, hasKind(app.RuntimePermissionRequest)), firstIndex(evs, task.EvGuardVerdict, verdictIs(policy.Allow))
		if ask < 0 || verdict < ask {
			t.Errorf("permission_request at %d, allow verdict at %d: want both, request first", ask, verdict)
		}
	})

	t.Run("a guarded command waits for approval and runs once approved", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Ask("execute", "cd work && rm victim.txt"), Write("out.txt", "x")}}))
		out := r.run(context.Background())
		id, approval := r.awaitApproval()
		if s := r.snap(id); s.State != task.WaitingApproval {
			t.Fatalf("state = %s, want waiting_approval", s.State)
		}
		if r.outExists() {
			t.Fatal("the runtime acted before it was approved")
		}
		if err := r.app.HandleApproval(context.Background(), id, approval, true); err != nil {
			t.Fatal(err)
		}
		if o := r.wait(out); o.err != nil {
			t.Fatal(o.err)
		}
		if s := r.snap(id); s.State != task.Done {
			t.Errorf("state = %s (%s), want done", s.State, s.Reason)
		}
		if !r.outExists() {
			t.Error("out.txt missing after approval")
		}
		evs := r.events(id)
		order := []int{
			firstIndex(evs, task.EvRuntimeEvent, hasKind(app.RuntimePermissionRequest)),
			firstIndex(evs, task.EvGuardVerdict, verdictIs(policy.Ask)),
			firstIndex(evs, task.EvApprovalRequested, any_),
			firstIndex(evs, task.EvState, hasState(task.WaitingApproval)),
			firstIndex(evs, task.EvApprovalDecided, any_),
			firstIndex(evs, task.EvContract, any_),
			slices.IndexFunc(evs, func(e task.Event) bool { return e.Type == task.EvState && hasState(task.Done)(e.Payload) }),
		}
		if !slices.IsSorted(order) || order[0] < 0 {
			t.Errorf("event order = %v, want request, verdict, approval requested, waiting, decided, contract, done", order)
		}
	})

	for _, rejectEnds := range []bool{false, true} {
		t.Run(fmt.Sprintf("a rejected request is not run (reject ends turn: %v)", rejectEnds), func(t *testing.T) {
			r := newRig(t, newRuntime(t, Script{RejectEndsTurn: rejectEnds, Steps: []Step{Ask("execute", "rm victim.txt"), Write("out.txt", "x")}}))
			out := r.run(context.Background())
			id, approval := r.awaitApproval()
			if err := r.app.HandleApproval(context.Background(), id, approval, false); err != nil {
				t.Fatal(err)
			}
			if o := r.wait(out); o.err != nil {
				t.Fatal(o.err)
			}
			s := r.snap(id)
			if s.State != task.Failed {
				t.Fatalf("state = %s, want failed: nothing changed, so the contract fails", s.State)
			}
			want := "no change in the workdir"
			if rejectEnds {
				want = "rejected request"
			}
			if !strings.Contains(s.Reason, want) {
				t.Errorf("reason = %q, want it to contain %q", s.Reason, want)
			}
			if r.outExists() {
				t.Error("the rejected action ran")
			}
		})
	}

	t.Run("a request the config denies is rejected without asking", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Ask("execute", "format-disk /dev/sda"), Write("out.txt", "x")}}))
		o := r.wait(r.run(context.Background()))
		if o.err != nil {
			t.Fatal(o.err)
		}
		if s := r.snap(o.id); s.State != task.Failed {
			t.Errorf("state = %s, want failed", s.State)
		}
		if n := len(r.notifier.Of(app.NoticeApproval)); n != 0 {
			t.Errorf("approval notices = %d, want 0", n)
		}
		if r.outExists() {
			t.Error("the denied action ran")
		}
		if firstIndex(r.events(o.id), task.EvGuardVerdict, verdictIs(policy.Deny)) < 0 {
			t.Error("no deny verdict in the event log")
		}
	})

	t.Run("cancel stops a running session", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Say("working"), Hang()}}))
		out := r.run(context.Background())
		id := r.taskID()
		r.eventually("session start", func() bool { return firstIndex(r.events(id), task.EvRuntimeSession, any_) >= 0 })
		if err := r.app.CancelDelegated(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if o := r.wait(out); o.err != nil {
			t.Fatal(o.err)
		}
		if s := r.snap(id); s.State != task.Failed || !strings.Contains(s.Reason, "cancelled") {
			t.Errorf("state = %s (%s), want failed: cancelled", s.State, s.Reason)
		}
	})

	t.Run("cancel while waiting for approval", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Ask("execute", "rm victim.txt"), Write("out.txt", "x")}}))
		out := r.run(context.Background())
		id, _ := r.awaitApproval()
		if err := r.app.CancelDelegated(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if o := r.wait(out); o.err != nil {
			t.Fatal(o.err)
		}
		if s := r.snap(id); s.State != task.Failed || !strings.Contains(s.Reason, "cancelled") {
			t.Errorf("state = %s (%s), want failed: cancelled", s.State, s.Reason)
		}
		if r.outExists() {
			t.Error("the pending action ran")
		}
	})

	t.Run("a runtime that dies mid-turn fails the task", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Say("half"), Crash()}}))
		o := r.wait(r.run(context.Background()))
		if o.err != nil {
			t.Fatal(o.err)
		}
		if s := r.snap(o.id); s.State != task.Failed {
			t.Errorf("state = %s, want failed", s.State)
		}
	})

	t.Run("a kernel crash mid-turn is reconciled to failed on restart", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Say("half"), Ask("execute", "go vet ./..."), Hang()}}))
		r.hook = func(p string) error {
			if p == "app.delegated_event" {
				return errCrash
			}
			return nil
		}
		o := r.wait(r.run(context.Background()))
		if !errors.Is(o.err, errCrash) {
			t.Fatalf("RunDelegated error = %v, want the simulated crash", o.err)
		}
		if s := r.snap(o.id); s.State != task.Running {
			t.Fatalf("state after crash = %s, want running", s.State)
		}
		r.hook = nil
		r.boot(newRuntime(t, Script{}))
		if err := r.app.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s := r.snap(o.id); s.State != task.Failed || !strings.Contains(s.Reason, "restart") {
			t.Errorf("state = %s (%s), want failed with a restart reason", s.State, s.Reason)
		}
	})

	t.Run("a kernel restart while waiting for approval fails the task", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Ask("execute", "rm victim.txt"), Write("out.txt", "x")}}))
		ctx, cancel := context.WithCancel(context.Background())
		out := r.run(ctx)
		id, _ := r.awaitApproval()
		cancel()
		r.wait(out)
		r.boot(newRuntime(t, Script{}))
		if err := r.app.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s := r.snap(id); s.State != task.Failed || !strings.Contains(s.Reason, "restart") {
			t.Errorf("state = %s (%s), want failed with a restart reason", s.State, s.Reason)
		}
		if err := r.app.HandleApproval(context.Background(), id, "late", true); err == nil {
			t.Error("a late approval was accepted")
		}
	})

	t.Run("the contract decides done", func(t *testing.T) {
		tests := []struct {
			name      string
			steps     []Step
			checks    []app.Check
			wantState task.State
			wantInfo  string
			wantRuns  int
		}{
			{"change and no checks", []Step{Write("out.txt", "x")}, nil, task.Done, "", 0},
			{"change and a passing check", []Step{Write("out.txt", "x")}, []app.Check{{Name: "go-test", Argv: []string{"pass"}, Timeout: time.Minute}}, task.Done, "", 1},
			{"no change", []Step{Say("all done")}, nil, task.Failed, "no change in the workdir", 0},
			{"change and a failing check", []Step{Write("out.txt", "x")}, []app.Check{{Name: "go-test", Argv: []string{"fail"}, Timeout: time.Minute}, {Name: "lint", Argv: []string{"pass"}}}, task.Failed, "check go-test failed (exit 1)", 1},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				r := newRig(t, newRuntime(t, Script{Steps: tc.steps}))
				o := r.wait(r.run(context.Background(), tc.checks...))
				if o.err != nil {
					t.Fatal(o.err)
				}
				s := r.snap(o.id)
				if s.State != tc.wantState || s.Reason != tc.wantInfo {
					t.Errorf("state = %s (%q), want %s (%q)", s.State, s.Reason, tc.wantState, tc.wantInfo)
				}
				runs := 0
				for _, e := range r.events(o.id) {
					if e.Type == task.EvCheckRun {
						runs++
					}
				}
				if runs != tc.wantRuns {
					t.Errorf("check events = %d, want %d", runs, tc.wantRuns)
				}
			})
		}
	})

	t.Run("message chunks are logged once", func(t *testing.T) {
		r := newRig(t, newRuntime(t, Script{Steps: []Step{Say("he", "llo ", "world"), Write("out.txt", "x")}}))
		o := r.wait(r.run(context.Background()))
		if o.err != nil {
			t.Fatal(o.err)
		}
		var texts []string
		for _, e := range r.events(o.id) {
			var ev loggedEvent
			if e.Type == task.EvRuntimeEvent && json.Unmarshal(e.Payload, &ev) == nil && ev.Kind == app.RuntimeMessage {
				texts = append(texts, ev.Text)
			}
		}
		if len(texts) != 1 || texts[0] != "hello world" {
			t.Errorf("message events = %q, want one \"hello world\"", texts)
		}
	})
}
