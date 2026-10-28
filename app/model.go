package app

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrNoModelSelector = errors.New("model switching is not configured")
	ErrUnknownModel    = errors.New("unknown model")
)

func (a *App) ListModels(ctx context.Context) ([]ModelChoice, error) {
	if a.d.ModelSelector == nil {
		return nil, ErrNoModelSelector
	}
	choices, err := a.d.ModelSelector.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	return choices, nil
}

func (a *App) CurrentModel() string {
	if a.d.ModelSelector == nil {
		return ""
	}
	return a.d.ModelSelector.Current()
}

// SelectModel is an owner command, not a tool write, so it is allowed while paused.
func (a *App) SelectModel(ctx context.Context, value string) error {
	if a.d.ModelSelector == nil {
		return ErrNoModelSelector
	}
	return a.SetSetting(ctx, "model", value)
}
