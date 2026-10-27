package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/panca/haythos/core/task"
)

const evModelSelected = task.EventType("kernel.model_selected")

var (
	ErrNoModelSelector = errors.New("model switching is not configured")
	ErrUnknownModel    = errors.New("unknown model")
)

type modelSelected struct {
	Model string `json:"model"`
}

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
	choices, err := a.ListModels(ctx)
	if err != nil {
		return err
	}
	known := false
	for _, c := range choices {
		if c.Value == value {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("select model %q: %w", value, ErrUnknownModel)
	}
	if err := a.d.ModelSelector.Select(ctx, value); err != nil {
		return fmt.Errorf("select model %q: %w", value, err)
	}
	if _, err := a.d.Events.Append(ctx, systemTask, evModelSelected, modelSelected{Model: value}); err != nil {
		return fmt.Errorf("record model selection: %w", err)
	}
	return nil
}
