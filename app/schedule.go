package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/panca/haythos/core/task"
)

type jobMark struct {
	Job string    `json:"job"`
	At  time.Time `json:"at"`
}

// Tick fires every cron job that became due since it last fired. After downtime it fires only the latest missed run.
func (a *App) Tick(ctx context.Context) error {
	now := a.d.Clock()
	sys, err := a.d.Events.Load(ctx, systemTask)
	if err != nil {
		return fmt.Errorf("load system events: %w", err)
	}
	last := map[string]time.Time{}
	for _, e := range sys {
		var m jobMark
		if (e.Type == evJobFired || e.Type == evJobArmed) && json.Unmarshal(e.Payload, &m) == nil && m.At.After(last[m.Job]) {
			last[m.Job] = m.At
		}
	}

	jobs, err := a.scheduledJobs(sys)
	if err != nil {
		return err
	}

	var errs []error
	if err := a.sweepApprovals(ctx); err != nil {
		errs = append(errs, err)
	}
	for _, j := range jobs {
		if j.Trigger.Cron == "" {
			continue
		}
		sched, err := cron.ParseStandard(j.Trigger.Cron)
		if err != nil {
			errs = append(errs, fmt.Errorf("job %s cron %q: %w", j.Name, j.Trigger.Cron, err))
			continue
		}
		from, armed := last[j.Name]
		if !armed {
			if _, err := a.d.Events.Append(ctx, systemTask, evJobArmed, jobMark{Job: j.Name, At: now}); err != nil {
				return fmt.Errorf("arm job %s: %w", j.Name, err)
			}
			continue
		}
		var due time.Time
		for t := sched.Next(from); !t.After(now); t = sched.Next(t) {
			due = t
		}
		if due.IsZero() {
			continue
		}
		id := fmt.Sprintf("%s-%d", j.Name, due.Unix())
		if _, err := a.createTask(ctx, id, j, "Run the scheduled job: "+j.Name, ""); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := a.d.Events.Append(ctx, systemTask, evJobFired, jobMark{Job: j.Name, At: due}); err != nil {
			return fmt.Errorf("record job %s fired: %w", j.Name, err)
		}
		if err := a.hook("app.after_job_fired"); err != nil {
			return err
		}
		if err := a.RunTask(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// sweepApprovals re-sends approval requests whose publish failed and fails tasks whose approval expired.
func (a *App) sweepApprovals(ctx context.Context) error {
	ids, err := a.d.Events.TasksIn(ctx, task.WaitingApproval)
	if err != nil {
		return fmt.Errorf("list waiting tasks: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if err := a.sweepApproval(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("sweep approval %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (a *App) sweepApproval(ctx context.Context, id string) error {
	unlock := a.lock(id)
	defer unlock()
	s, err := a.Snapshot(ctx, id)
	if err != nil || s.State != task.WaitingApproval {
		return err
	}
	events, err := a.d.Events.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("load task %s: %w", id, err)
	}
	var rec approvalRequested
	for _, e := range events {
		var r approvalRequested
		if e.Type == task.EvApprovalRequested && json.Unmarshal(e.Payload, &r) == nil {
			rec = r
		}
	}
	if rec.Approval.ID == "" {
		return nil
	}
	if a.d.Clock().Before(rec.Approval.Expiry) {
		return a.notifyApproval(ctx, id, rec)
	}
	if err := a.setState(ctx, id, task.Running, "approval expired"); err != nil {
		return err
	}
	if err := a.appendResult(ctx, id, task.ToolResult{CallID: rec.CallID, Content: "approval expired", IsError: true}); err != nil {
		return err
	}
	return a.fail(ctx, id, "approval expired")
}
