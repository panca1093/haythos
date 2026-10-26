package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
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

	var errs []error
	for _, j := range a.d.Jobs {
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
		if _, err := a.createTask(ctx, id, j, "Run the scheduled job: "+j.Name); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, err := a.d.Events.Append(ctx, systemTask, evJobFired, jobMark{Job: j.Name, At: due}); err != nil {
			return fmt.Errorf("record job %s fired: %w", j.Name, err)
		}
		if err := a.RunTask(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("run %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
