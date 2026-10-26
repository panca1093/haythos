package app

import (
	"context"
	"errors"
	"time"

	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

var ErrNotImplemented = errors.New("not implemented")

type Deps struct {
	Models      map[string]Model
	DefaultJob  Job
	Jobs        []Job
	Tools       Tools
	Events      EventStore
	Ledger      Ledger
	Notifier    Notifier
	Vault       Vault
	Policy      policy.Policy
	Clock       func() time.Time
	ApprovalTTL time.Duration
	// Hook is called at named crash points; a non-nil error aborts the operation as if the process died.
	Hook func(point string) error
}

type App struct{ d Deps }

func New(d Deps) *App { return &App{d: d} }

func (a *App) Start(ctx context.Context) error { return ErrNotImplemented }

func (a *App) CreateTask(ctx context.Context, job Job, input string) (string, error) {
	return "", ErrNotImplemented
}

func (a *App) RunTask(ctx context.Context, taskID string) error { return ErrNotImplemented }

func (a *App) HandleInput(ctx context.Context, in Input) (string, error) {
	return "", ErrNotImplemented
}

func (a *App) HandleApproval(ctx context.Context, taskID, approvalID string, approve bool) error {
	return ErrNotImplemented
}

func (a *App) RunJob(ctx context.Context, name string) (string, error) {
	return "", ErrNotImplemented
}

func (a *App) Tick(ctx context.Context) error { return ErrNotImplemented }

func (a *App) Pause(ctx context.Context) error { return ErrNotImplemented }

func (a *App) Resume(ctx context.Context) error { return ErrNotImplemented }

func (a *App) Snapshot(ctx context.Context, taskID string) (task.Snapshot, error) {
	return task.Snapshot{}, ErrNotImplemented
}
