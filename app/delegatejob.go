package app

import (
	"context"
	"fmt"
	"log/slog"
)

func delegationOf(j Job) Delegation {
	return Delegation{Runtime: j.Runtime, Prompt: j.Goal, Workdir: j.Workdir, Model: j.RuntimeModel, Mode: j.RuntimeMode, Checks: j.Checks}
}

// launchDelegated creates the task of a scheduled delegated job and runs its session in the background, so the tick that fired it is not held.
// A task that already exists was launched by an earlier process; startup fails its lost session.
func (a *App) launchDelegated(ctx context.Context, id string, j Job) error {
	d := delegationOf(j)
	rt, err := a.delegateTarget(d)
	if err != nil {
		return err
	}
	prior, err := a.d.Events.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("check task %s: %w", id, err)
	}
	if len(prior) > 0 {
		return nil
	}
	if _, err := a.createTask(ctx, id, j, d.Prompt, d.ReplyTo, d.Workdir); err != nil {
		return err
	}
	a.bg.Add(1)
	go func() {
		defer a.bg.Done()
		if err := a.runDelegated(ctx, id, rt, d); err != nil {
			slog.Error("delegated job failed", "task", id, "job", j.Name, "err", err)
		}
	}()
	return nil
}

// Wait blocks until every background delegated session has ended.
func (a *App) Wait() { a.bg.Wait() }
