package app

import (
	"context"
	"errors"
	"time"

	"github.com/panca/haythos/core/task"
)

var (
	ErrInvalidJob = errors.New("invalid job")
	ErrUnknownJob = errors.New("unknown job")
)

type JobDef struct {
	Name   string      `json:"name"`
	Cron   string      `json:"cron"`
	Goal   string      `json:"goal"`
	Tools  []string    `json:"tools"`
	Model  string      `json:"model,omitempty"`
	Budget task.Budget `json:"budget,omitempty"`
}

type JobStatus struct {
	Name    string
	Source  string
	Cron    string
	Tools   []string
	Enabled bool
	Next    time.Time
}

func (a *App) DefineJob(ctx context.Context, def JobDef) error { return errors.New("not implemented") }

func (a *App) SetJobEnabled(ctx context.Context, name string, enabled bool) error {
	return errors.New("not implemented")
}

func (a *App) DeleteJob(ctx context.Context, name string) error { return errors.New("not implemented") }

func (a *App) ListJobs(ctx context.Context) ([]JobStatus, error) {
	return nil, errors.New("not implemented")
}
