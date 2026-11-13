//go:build integration

package acceptance_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/config"
	"github.com/panca/haythos/adapters/runtime/claudecli"
	"github.com/panca/haythos/adapters/runtime/workspace"
	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

// TestRealClaudeDelegatedJob drives a manual job through the app with the real claude CLI (haiku) in a scratch repo.
func TestRealClaudeDelegatedJob(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"README.md": "# scratch\n", "victim.txt": "delete me\n"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.test", "commit", "-qm", "init"}} {
		c := exec.CommandContext(context.Background(), "git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	rt := config.Runtime{Kind: "claudecli", Command: "claude", Model: "haiku", Workdirs: map[string]string{"scratch": repo}}
	dir := t.TempDir()
	job := `name: it
goal: >
  Do exactly two things. First append the line "hello from haythos" to README.md with the Edit tool.
  Second run the shell command "rm victim.txt". Then reply "done" and nothing else.
trigger: { manual: true }
runtime: { alias: claude, workdir: scratch }
checks:
  - { name: readme, argv: [grep, -q, hello from haythos, README.md], timeout: 1m }
`
	if err := os.WriteFile(filepath.Join(dir, "it.yaml"), []byte(job), 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, err := config.LoadJobs(dir, map[string]config.Runtime{"claude": rt})
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
	n := &testfakes.Notifier{}
	a := app.New(app.Deps{
		Models: map[string]app.Model{}, Events: st, Ledger: st, Notifier: n, Vault: testfakes.Vault{}, Jobs: jobs,
		Runtimes: map[string]app.Runtime{"claude": claudecli.New(claudecli.Config{Command: rt.Command, Model: rt.Model})},
		Guards:   map[string]policy.Guard{"claude": guard}, WorkdirState: workspace.State, RunCheck: workspace.RunCheck,
		ApprovalTTL: time.Hour,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.RunJob(ctx, "it"); done <- err }()
	approved := map[string]bool{}
	for finished := false; !finished; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			finished = true
		case <-time.After(100 * time.Millisecond):
			for _, ap := range n.Of(app.NoticeApproval) {
				if !approved[ap.Approval.Approval.ID] {
					approved[ap.Approval.Approval.ID] = true
					t.Logf("approving %s", ap.Approval.Preview)
					if err := a.HandleApproval(ctx, ap.TaskID, ap.Approval.Approval.ID, true); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	evs, err := st.Since(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, e := range evs {
		if e.Type == task.EvCreated && e.TaskID != "system" {
			id = e.TaskID
		}
	}
	s, err := a.Snapshot(ctx, id)
	if err != nil || s.State != task.Done {
		t.Fatalf("state = %s (%s), err = %v, want done", s.State, s.Reason, err)
	}
	if len(approved) == 0 {
		t.Error("no approval was asked for the rm command")
	}
	if _, err := os.Stat(filepath.Join(repo, "victim.txt")); err == nil {
		t.Error("victim.txt still exists after the approved rm")
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "README.md")); !strings.Contains(string(b), "hello from haythos") {
		t.Errorf("README = %q", b)
	}
}
