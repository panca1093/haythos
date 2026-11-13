package app

import (
	"context"
	"errors"
	"time"
)

var ErrDelegated = errors.New("task is delegated to a runtime")

// Check is a command the kernel runs after the session; Argv is never a shell string.
type Check struct {
	Name    string
	Argv    []string
	Timeout time.Duration
}

type Delegation struct {
	// Runtime is an alias in Deps.Runtimes.
	Runtime string
	Prompt  string
	Workdir string
	Model   string
	Mode    string
	Checks  []Check
	ReplyTo string
}

// RunDelegated runs job on a runtime and blocks until the task is done or failed.
func (a *App) RunDelegated(_ context.Context, _ Job, _ Delegation) (string, error) {
	return "", errors.New("not implemented")
}

// CancelDelegated stops the live session of a delegated task; the task then fails as cancelled.
func (a *App) CancelDelegated(_ context.Context, _ string) error {
	return errors.New("not implemented")
}
