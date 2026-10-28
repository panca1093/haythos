package app

import (
	"context"
	"errors"
	"time"
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

func (a *App) SetSetting(ctx context.Context, key, value string) error {
	return errors.New("not implemented")
}

func (a *App) ResetSetting(ctx context.Context, key string) error {
	return errors.New("not implemented")
}

func (a *App) Settings(ctx context.Context) ([]SettingValue, error) {
	return nil, errors.New("not implemented")
}

func (a *App) SettingHistory(ctx context.Context, key string) ([]SettingChange, error) {
	return nil, errors.New("not implemented")
}
