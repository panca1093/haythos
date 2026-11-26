package app

import (
	"context"
	"errors"
)

var ErrNothingRunning = errors.New("nothing is running")

func (a *App) Interrupt(ctx context.Context, replyTo string) error {
	return ErrNothingRunning
}
