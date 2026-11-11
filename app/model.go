package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/panca/haythos/core/task"
)

var (
	ErrNoModelSelector = errors.New("model switching is not configured")
	ErrUnknownModel    = errors.New("unknown model")
	ErrModelDiscovery  = errors.New("could not discover plan models")
)

const evConversationModel = task.EventType("conv.model")

func (a *App) ChatModels(ctx context.Context) ([]ModelChoice, error) {
	type catalog struct {
		alias   string
		choices []ModelChoice
		err     error
	}
	var choices []ModelChoice
	results := make(chan catalog, len(a.d.Models))
	count := 0
	for name, model := range a.d.Models {
		if a.blockedChatModel(name) {
			continue
		}
		choices = append(choices, ModelChoice{Value: name, Name: name})
		if variants, ok := model.(ModelVariants); ok {
			count++
			go func() {
				list, err := variants.List(ctx)
				results <- catalog{alias: name, choices: list, err: err}
			}()
		}
	}
	var listErr error
collect:
	for range count {
		var result catalog
		select {
		case result = <-results:
		case <-ctx.Done():
			select {
			case result = <-results:
			default:
				listErr = errors.Join(listErr, ctx.Err())
				break collect
			}
		}
		if result.err != nil {
			listErr = errors.Join(listErr, fmt.Errorf("%s: %w", result.alias, result.err))
		}
		for _, choice := range result.choices {
			if choice.Value == "" {
				continue
			}
			choice.Value = result.alias + "/" + choice.Value
			if !a.blockedChatModel(choice.Value) {
				choices = append(choices, choice)
			}
		}
	}
	slices.SortFunc(choices, func(a, b ModelChoice) int { return strings.Compare(a.Value, b.Value) })
	return slices.CompactFunc(choices, func(a, b ModelChoice) bool { return a.Value == b.Value }), listErr
}

func (a *App) SetConversationModel(ctx context.Context, replyTo, model string) error {
	if err := a.validateChatModel(ctx, model); err != nil {
		return err
	}
	if _, err := a.d.Events.Append(ctx, convStream(replyTo), evConversationModel, model); err != nil {
		return fmt.Errorf("save conversation model: %w", err)
	}
	return nil
}

func (a *App) validateChatModel(ctx context.Context, name string) error {
	if a.blockedChatModel(name) {
		return fmt.Errorf("%w %q: alias is reserved for subscription chat", ErrUnknownModel, name)
	}
	if _, ok := a.d.Models[name]; ok {
		return nil
	}
	variants, id, ok := a.modelVariant(name)
	if !ok {
		return fmt.Errorf("%w %q", ErrUnknownModel, name)
	}
	choices, err := variants.List(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrModelDiscovery, err)
	}
	if !slices.ContainsFunc(choices, func(c ModelChoice) bool { return c.Value == id }) {
		return fmt.Errorf("%w %q", ErrUnknownModel, name)
	}
	return nil
}

func (a *App) blockedChatModel(name string) bool {
	alias, _, _ := strings.Cut(name, "/")
	if alias == a.d.BlockedChatModel && alias != "" {
		return true
	}
	_, configured := a.d.Models[name]
	_, _, variant := a.modelVariant(name)
	return configured && variant
}

func (a *App) modelVariant(name string) (ModelVariants, string, bool) {
	alias, id, hasVariant := strings.Cut(name, "/")
	variants, ok := a.d.Models[alias].(ModelVariants)
	return variants, id, hasVariant && id != "" && ok
}

func (a *App) resolveModel(name string) (Model, bool) {
	if m, ok := a.d.Models[name]; ok {
		return m, true
	}
	if variants, id, ok := a.modelVariant(name); ok {
		return variants.WithModel(id), true
	}
	return nil, false
}

func (a *App) ConversationModel(ctx context.Context, replyTo string) (string, error) {
	stream := convStream(replyTo)
	events, err := a.d.Events.Load(ctx, stream)
	if err != nil {
		return "", fmt.Errorf("load conversation model: %w", err)
	}
	return conversationModel(events, stream)
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
	if a.d.ModelSelector == nil {
		return ErrNoModelSelector
	}
	return a.SetSetting(ctx, "model", value)
}
