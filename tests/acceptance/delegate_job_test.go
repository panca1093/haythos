package acceptance_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/config"
	"github.com/panca/haythos/adapters/runtime/workspace"
	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type jobRig struct {
	t        *testing.T
	repo     string
	clock    *testfakes.Clock
	notifier *testfakes.Notifier
	store    *sqlite.Store
	app      *app.App
	runtime  *testfakes.Runtime
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-qm", "first", "--allow-empty"}} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// newJobRig loads jobYAML through the real config loader and delegates to a scripted runtime with the real workdir probe and check runner.
func newJobRig(t *testing.T, jobYAML string, steps func(repo string) []testfakes.RuntimeStep) *jobRig {
	t.Helper()
	repo := gitRepo(t)
	rt := config.Runtime{Kind: "acp", Command: "unused", Workdirs: map[string]string{"repo": repo},
		Guard: []policy.GuardRule{{Name: "no-format", Pattern: `format-disk`, Action: policy.Deny}}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "job.yaml"), []byte(jobYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := config.LoadJobs(dir, map[string]config.Runtime{"fake": rt})
	if err != nil {
		t.Fatal(err)
	}
	guard, err := policy.NewGuard(rt.GuardRules())
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	r := &jobRig{t: t, repo: repo, clock: testfakes.NewClock(time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)), notifier: &testfakes.Notifier{}, store: st}
	r.runtime = testfakes.NewRuntime("fake", testfakes.RuntimeScript{Steps: steps(repo)})
	r.app = app.New(app.Deps{
		Models: map[string]app.Model{}, Events: st, Ledger: st, Notifier: r.notifier, Vault: testfakes.Vault{},
		Clock: r.clock.Now, ApprovalTTL: time.Hour, Jobs: jobs,
		Runtimes: map[string]app.Runtime{"fake": r.runtime}, Guards: map[string]policy.Guard{"fake": guard},
		WorkdirState: workspace.State, RunCheck: workspace.RunCheck,
	})
	return r
}

func writeInRepo(name, body string) testfakes.RuntimeStep {
	return testfakes.Do(func(workdir string, allowed bool) {
		if allowed {
			_ = os.WriteFile(filepath.Join(workdir, name), []byte(body), 0o600)
		}
	})
}

func (r *jobRig) runJobAsync(name string) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := r.app.RunJob(context.Background(), name); done <- err }()
	return done
}

func (r *jobRig) waitApproval() app.Notice {
	r.t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if n := r.notifier.Of(app.NoticeApproval); len(n) > 0 {
			return n[0]
		}
	}
	r.t.Fatal("no approval notice")
	return app.Notice{}
}

func (r *jobRig) onlyTask() string {
	r.t.Helper()
	evs, err := r.store.Since(context.Background(), 0)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == task.EvCreated && e.TaskID != "system" {
			return e.TaskID
		}
	}
	r.t.Fatal("no task created")
	return ""
}

func (r *jobRig) events(id string, typ task.EventType) []json.RawMessage {
	r.t.Helper()
	evs, err := r.store.Load(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []json.RawMessage
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e.Payload)
		}
	}
	return out
}

const manualJob = `name: fix
goal: Add hello.txt.
trigger: { manual: true }
runtime: { alias: fake, workdir: repo }
checks:
  - { name: has-file, argv: [sh, -c, "%s"], timeout: 30s }
`

func manual(check string) string { return strings.Replace(manualJob, "%s", check, 1) }

func guardedSteps(string) []testfakes.RuntimeStep {
	return []testfakes.RuntimeStep{askStep("q1", "execute", "rm -f scratch.tmp"), writeInRepo("hello.txt", "hello\n")}
}

func TestManualDelegatedJobApprovesRunsChecksAndRecordsTheDiff(t *testing.T) {
	r := newJobRig(t, manual("test -f hello.txt && echo CHECK-OK"), guardedSteps)
	done := r.runJobAsync("fix")
	n := r.waitApproval()
	if !strings.Contains(n.Approval.Preview, "rm -f scratch.tmp") || !strings.Contains(n.Approval.Preview, "delete") {
		t.Errorf("approval preview = %q, want the command text and the rule", n.Approval.Preview)
	}
	if err := r.app.HandleApproval(context.Background(), n.TaskID, n.Approval.Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	id := r.onlyTask()
	s, err := r.app.Snapshot(context.Background(), id)
	if err != nil || s.State != task.Done {
		t.Fatalf("state = %s (%s) err = %v, want done", s.State, s.Reason, err)
	}
	if !s.Tainted {
		t.Error("task is untainted, want it tainted by the check output")
	}
	if calls := r.runtime.Session(0).Approvals(); len(calls) != 1 || calls[0].OptionID != "allow-q1" {
		t.Errorf("approvals = %+v, want allow-q1", calls)
	}
	if checks := r.events(id, task.EvCheckRun); len(checks) != 1 || !strings.Contains(string(checks[0]), "CHECK-OK") {
		t.Errorf("check events = %s, want one with the output", checks)
	}
	var v struct {
		Pass          bool
		Before, After string
	}
	contract := r.events(id, task.EvContract)
	if len(contract) != 1 || json.Unmarshal(contract[0], &v) != nil || !v.Pass || v.Before == "" || v.After == "" || v.Before == v.After {
		t.Errorf("contract event = %s, want a pass with different before and after fingerprints", contract)
	}
	final := r.notifier.Of(app.NoticeMessage)
	if len(final) != 1 || !final[0].Tainted || !strings.Contains(final[0].Text, "has-file") {
		t.Errorf("final notices = %+v, want one tainted notice that names the checks", final)
	}
}

func TestManualDelegatedJobRejectedGuardedActionFailsWithoutAChange(t *testing.T) {
	r := newJobRig(t, manual("true"), guardedSteps)
	done := r.runJobAsync("fix")
	n := r.waitApproval()
	if err := r.app.HandleApproval(context.Background(), n.TaskID, n.Approval.Approval.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s, _ := r.app.Snapshot(context.Background(), r.onlyTask())
	if s.State != task.Failed || !strings.Contains(s.Reason, "no change") {
		t.Errorf("state = %s (%s), want failed with no change in the workdir", s.State, s.Reason)
	}
}

func TestManualDelegatedJobFailingCheckFailsTheTask(t *testing.T) {
	r := newJobRig(t, manual("echo BROKEN-TEST; exit 1"), func(string) []testfakes.RuntimeStep {
		return []testfakes.RuntimeStep{writeInRepo("hello.txt", "hello\n")}
	})
	if err := <-r.runJobAsync("fix"); err != nil {
		t.Fatal(err)
	}
	id := r.onlyTask()
	s, _ := r.app.Snapshot(context.Background(), id)
	if s.State != task.Failed || s.Reason != "check has-file failed (exit 1)" {
		t.Errorf("state = %s (%s), want failed on the check", s.State, s.Reason)
	}
	if checks := r.events(id, task.EvCheckRun); len(checks) != 1 || !strings.Contains(string(checks[0]), "BROKEN-TEST") {
		t.Errorf("check events = %s, want the failing output recorded", checks)
	}
	if len(r.notifier.Of(app.NoticeApproval)) != 0 {
		t.Error("an unguarded run asked for approval")
	}
}

func TestGuardRulesOfTheAliasApply(t *testing.T) {
	r := newJobRig(t, manual("true"), func(string) []testfakes.RuntimeStep {
		return []testfakes.RuntimeStep{askStep("q1", "execute", "format-disk /dev/x"), writeInRepo("hello.txt", "hello\n")}
	})
	if err := <-r.runJobAsync("fix"); err != nil {
		t.Fatal(err)
	}
	if calls := r.runtime.Session(0).Approvals(); len(calls) != 1 || calls[0].OptionID != "reject-q1" {
		t.Errorf("approvals = %+v, want the owner's deny rule to reject", calls)
	}
}

func TestCronDelegatedJobRunsInTheBackground(t *testing.T) {
	r := newJobRig(t, strings.Replace(manual("true"), "trigger: { manual: true }", `trigger: { cron: "CRON_TZ=UTC 0 0 * * *" }`, 1), func(string) []testfakes.RuntimeStep {
		return []testfakes.RuntimeStep{writeInRepo("hello.txt", "hello\n")}
	})
	ctx := context.Background()
	if err := r.app.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(24 * time.Hour)
	tickReturned := make(chan error, 1)
	go func() { tickReturned <- r.app.Tick(ctx) }()
	select {
	case err := <-tickReturned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Tick blocked on the delegated session")
	}
	r.app.Wait()
	s, _ := r.app.Snapshot(ctx, r.onlyTask())
	if s.State != task.Done {
		t.Errorf("state = %s (%s), want done", s.State, s.Reason)
	}
	if err := r.app.Tick(ctx); err != nil || r.runtime.Sessions() != 1 {
		t.Errorf("tick again: err = %v, sessions = %d, want the fired job not to start again", err, r.runtime.Sessions())
	}
}
