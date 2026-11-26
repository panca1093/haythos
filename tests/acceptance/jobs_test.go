package acceptance_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/panca/haythos/core/task"
)

// M2-4. A job defined from chat needs approval; before it, it does not exist.
func TestChatJobNeedsApprovalThenFires(t *testing.T) {
	r := newM2Rig(t, callTurn("c1", "jobs.define", diskJobArgs), finalTurn("Job disk-check is set."), finalTurn("Disk is at 40%."))
	id := r.chat("every Monday 09:00 check the disk and report")
	if got := r.snap(id).State; got != task.WaitingApproval {
		t.Fatalf("state = %s, want waiting_approval", got)
	}
	if got := r.jobNames(); !cmp.Equal(got, []string{"vm-health"}) {
		t.Fatalf("jobs before approval = %v", got)
	}
	r.tick()
	if got := r.reports(); len(got) != 0 {
		t.Fatalf("reports before approval = %q", got)
	}

	r.approveLast(id)
	if got := r.jobNames(); !cmp.Equal(got, []string{"vm-health", "disk-check"}) {
		t.Fatalf("jobs after approval = %v", got)
	}
	r.tick()
	r.untilMonday9()
	r.tick()
	got := r.reports()
	if len(got) != 2 || got[1] != "Disk is at 40%." {
		t.Fatalf("reports = %q, want the chat reply then the disk-check run", got)
	}
}

// M2-5. A chat job survives restart and fires once even if the process dies between firing and running.
func TestChatJobSurvivesRestartNoDuplicateRun(t *testing.T) {
	r := newM2Rig(t, callTurn("c1", "jobs.define", diskJobArgs), finalTurn("Job set."))
	r.approveLast(r.chat("every Monday 09:00 check the disk"))

	r.boot()
	r.start()
	if got := r.jobNames(); !cmp.Equal(got, []string{"vm-health", "disk-check"}) {
		t.Fatalf("jobs after restart = %v", got)
	}
	r.untilMonday9()
	r.hook = func(p string) error {
		if p == "app.after_job_fired" {
			return errCrash
		}
		return nil
	}
	if err := r.app.Tick(context.Background()); !errors.Is(err, errCrash) {
		t.Fatalf("tick: want crash, got %v", err)
	}
	r.hook = nil
	r.boot(finalTurn("Disk is at 40%."))
	r.start()
	r.tick()
	r.tick()
	var runs int
	for _, rep := range r.reports() {
		if rep == "Disk is at 40%." {
			runs++
		}
	}
	if runs != 1 {
		t.Errorf("disk-check reports = %d, want exactly 1 (reports %q)", runs, r.reports())
	}
}

// M2-6. Invalid job definitions are rejected with an error the model can read, and define nothing.
func TestInvalidJobDefinitionRejected(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"unknown tool", `{"name":"a","cron":"0 9 * * *","goal":"g","tools":["nope.read"]}`},
		{"tool above tier 1", `{"name":"a","cron":"0 9 * * *","goal":"g","tools":["msg.send"]}`},
		{"job creating jobs", `{"name":"a","cron":"0 9 * * *","goal":"g","tools":["jobs.define"]}`},
		{"invalid cron", `{"name":"a","cron":"every monday","goal":"g","tools":["vm.read"]}`},
		{"config job name", `{"name":"vm-health","cron":"0 9 * * *","goal":"g","tools":["vm.read"]}`},
		{"name not a slug", `{"name":"Disk Check!","cron":"0 9 * * *","goal":"g","tools":["vm.read"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newM2Rig(t, callTurn("c1", "jobs.define", tc.args), finalTurn("could not create it"))
			id := r.chat("make a job")
			if r.snap(id).State == task.WaitingApproval {
				r.approveLast(id)
			}
			var res *task.ToolResult
			for _, m := range r.snap(id).History {
				for i := range m.Results {
					if m.Results[i].CallID == "c1" {
						res = &m.Results[i]
					}
				}
			}
			if res == nil || !res.IsError || res.Content == "" {
				t.Fatalf("jobs.define result = %+v, want a readable error", res)
			}
			if got := r.jobNames(); !cmp.Equal(got, []string{"vm-health"}) {
				t.Errorf("jobs = %v, want only the config job", got)
			}
		})
	}
}

// M2-7. Disabled or deleted jobs do not fire; re-enabling does not replay missed runs.
func TestDisabledAndDeletedJobsDoNotFire(t *testing.T) {
	ctx := context.Background()
	r := newM2Rig(t, callTurn("c1", "jobs.define", diskJobArgs), finalTurn("Job set."), finalTurn("Disk is at 40%."))
	r.approveLast(r.chat("every Monday 09:00 check the disk"))
	r.tick()

	if err := r.app.SetJobEnabled(ctx, "disk-check", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	r.untilMonday9()
	r.tick()
	if err := r.app.SetJobEnabled(ctx, "disk-check", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	r.tick()
	if got := len(r.reports()); got != 1 {
		t.Fatalf("reports = %q, want only the chat reply (missed run not replayed)", r.reports())
	}

	r.clock.Advance(7 * 24 * time.Hour)
	r.tick()
	if got := r.reports(); len(got) != 2 {
		t.Fatalf("reports after re-enable = %q, want the next Monday run", got)
	}

	if err := r.app.DeleteJob(ctx, "disk-check"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	r.clock.Advance(7 * 24 * time.Hour)
	r.tick()
	if got := len(r.reports()); got != 2 {
		t.Errorf("reports after delete = %q, want no new run", r.reports())
	}
	if got := r.jobNames(); !cmp.Equal(got, []string{"vm-health"}) {
		t.Errorf("jobs after delete = %v", got)
	}
}
