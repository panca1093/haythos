package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/panca/haythos/core/task"
)

const (
	evSettingChanged = task.EventType("setting.changed")
	evSettingReset   = task.EventType("setting.reset")
)

var (
	ErrUnknownSetting = errors.New("unknown setting")
	ErrInvalidSetting = errors.New("invalid setting value")
)

type SettingValue struct {
	Key        string
	Value      string
	Default    string
	Overridden bool
}

type SettingChange struct {
	Key   string
	Value string
	Reset bool
	At    time.Time
}

// setting is a runtime-changeable key; the config file holds def, the _system stream holds overrides.
type setting struct {
	def      string
	validate func(ctx context.Context, value string) error
	apply    func(ctx context.Context, value string) error
}

type settingEvent struct {
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

func (a *App) registerSettings() {
	a.settings = map[string]setting{}
	if sel := a.d.ModelSelector; sel != nil {
		a.settings["model"] = setting{def: sel.Current(), validate: a.validModel, apply: sel.Select}
	}
}

func (a *App) validModel(ctx context.Context, value string) error {
	choices, err := a.ListModels(ctx)
	if err != nil {
		return err
	}
	for _, c := range choices {
		if c.Value == value {
			return nil
		}
	}
	return fmt.Errorf("%w: %w %q", ErrInvalidSetting, ErrUnknownModel, value)
}

// SetSetting is an owner command, not a tool write, so it is allowed while paused.
func (a *App) SetSetting(ctx context.Context, key, value string) error {
	s, ok := a.settings[key]
	if !ok {
		return fmt.Errorf("set %q: %w", key, ErrUnknownSetting)
	}
	if err := s.validate(ctx, value); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	if err := s.apply(ctx, value); err != nil {
		return fmt.Errorf("apply %s: %w", key, err)
	}
	if _, err := a.d.Events.Append(ctx, systemTask, evSettingChanged, settingEvent{Key: key, Value: value}); err != nil {
		return fmt.Errorf("record setting %s: %w", key, err)
	}
	return nil
}

func (a *App) ResetSetting(ctx context.Context, key string) error {
	s, ok := a.settings[key]
	if !ok {
		return fmt.Errorf("reset %q: %w", key, ErrUnknownSetting)
	}
	if err := s.apply(ctx, s.def); err != nil {
		return fmt.Errorf("apply %s: %w", key, err)
	}
	if _, err := a.d.Events.Append(ctx, systemTask, evSettingReset, settingEvent{Key: key}); err != nil {
		return fmt.Errorf("record reset %s: %w", key, err)
	}
	return nil
}

func (a *App) SettingHistory(ctx context.Context, key string) ([]SettingChange, error) {
	if _, ok := a.settings[key]; !ok {
		return nil, fmt.Errorf("history %q: %w", key, ErrUnknownSetting)
	}
	all, err := a.settingChanges(ctx)
	if err != nil {
		return nil, err
	}
	var out []SettingChange
	for _, c := range all {
		if c.Key == key {
			out = append(out, c)
		}
	}
	return out, nil
}

func (a *App) Settings(ctx context.Context) ([]SettingValue, error) {
	over, err := a.overrides(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SettingValue, 0, len(a.settings))
	for k, s := range a.settings {
		v, ok := over[k]
		if !ok {
			v = s.def
		}
		out = append(out, SettingValue{Key: k, Value: v, Default: s.def, Overridden: ok})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// RestoreSettings applies persisted overrides. Start calls it; one-shot commands that skip Start must too.
func (a *App) RestoreSettings(ctx context.Context) error {
	over, err := a.overrides(ctx)
	if err != nil {
		return err
	}
	for k, v := range over {
		if s, ok := a.settings[k]; ok {
			if err := s.apply(ctx, v); err != nil {
				return fmt.Errorf("restore setting %s: %w", k, err)
			}
		}
	}
	return nil
}

func (a *App) overrides(ctx context.Context) (map[string]string, error) {
	all, err := a.settingChanges(ctx)
	if err != nil {
		return nil, err
	}
	over := map[string]string{}
	for _, c := range all {
		if c.Reset {
			delete(over, c.Key)
		} else {
			over[c.Key] = c.Value
		}
	}
	return over, nil
}

func (a *App) settingChanges(ctx context.Context) ([]SettingChange, error) {
	sys, err := a.d.Events.Load(ctx, systemTask)
	if err != nil {
		return nil, fmt.Errorf("load system events: %w", err)
	}
	var out []SettingChange
	for _, e := range sys {
		if e.Type != evSettingChanged && e.Type != evSettingReset {
			continue
		}
		var p settingEvent
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fmt.Errorf("decode setting event %d: %w", e.Cursor, err)
		}
		out = append(out, SettingChange{Key: p.Key, Value: p.Value, Reset: e.Type == evSettingReset, At: e.TS})
	}
	return out, nil
}
