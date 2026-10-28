package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/panca/haythos/core/task"
)

const (
	evThreadActivated   = task.EventType("thread.activated")
	evThreadMentionOnly = task.EventType("thread.mention_only")
)

// ThreadMode is what the channel adapter remembers about a chat thread.
type ThreadMode struct {
	Active      bool
	MentionOnly bool
}

type threadEvent struct {
	ID          string `json:"id"`
	MentionOnly bool   `json:"mention_only,omitempty"`
}

func (a *App) ActivateThread(ctx context.Context, id string) error {
	modes, err := a.ThreadModes(ctx)
	if err != nil {
		return err
	}
	if modes[id].Active {
		return nil
	}
	if _, err := a.d.Events.Append(ctx, systemTask, evThreadActivated, threadEvent{ID: id}); err != nil {
		return fmt.Errorf("record thread %s activation: %w", id, err)
	}
	return nil
}

func (a *App) SetThreadMentionOnly(ctx context.Context, id string, on bool) error {
	if _, err := a.d.Events.Append(ctx, systemTask, evThreadMentionOnly, threadEvent{ID: id, MentionOnly: on}); err != nil {
		return fmt.Errorf("record thread %s mention-only: %w", id, err)
	}
	return nil
}

func (a *App) ThreadModes(ctx context.Context) (map[string]ThreadMode, error) {
	sys, err := a.d.Events.Load(ctx, systemTask)
	if err != nil {
		return nil, fmt.Errorf("load system events: %w", err)
	}
	out := map[string]ThreadMode{}
	for _, e := range sys {
		if e.Type != evThreadActivated && e.Type != evThreadMentionOnly {
			continue
		}
		var p threadEvent
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode thread event %d: %w", e.Cursor, err)
		}
		m := out[p.ID]
		if e.Type == evThreadActivated {
			m.Active = true
		} else {
			m.MentionOnly = p.MentionOnly
		}
		out[p.ID] = m
	}
	return out, nil
}
