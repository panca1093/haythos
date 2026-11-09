package acceptance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/panca/haythos/app"
)

// M2-1. Settings survive a restart, the latest change wins, reset returns to the default, history is ordered.
func TestSettingsPersistLatestWinsAndReset(t *testing.T) {
	ctx := context.Background()
	r := newM2Rig(t)
	for _, v := range []string{"sonnet", "haiku"} {
		if err := r.app.SetSetting(ctx, "model", v); err != nil {
			t.Fatalf("set model %s: %v", v, err)
		}
	}
	r.boot()
	r.start()
	if got := r.setting("model"); got.Value != "haiku" || got.Default != "default" || !got.Overridden {
		t.Fatalf("after restart model = %+v, want haiku overriding default", got)
	}
	if err := r.app.ResetSetting(ctx, "model"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	r.boot()
	r.start()
	if got := r.setting("model"); got.Value != "default" || got.Overridden {
		t.Fatalf("after reset + restart model = %+v, want the default", got)
	}
	hist, err := r.app.SettingHistory(ctx, "model")
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	type step struct {
		Value string
		Reset bool
	}
	var got []step
	for _, h := range hist {
		got = append(got, step{h.Value, h.Reset})
	}
	want := []step{{"sonnet", false}, {"haiku", false}, {"", true}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("history (-want +got):\n%s", diff)
	}
}

// M2-2. An unknown key or an invalid value is rejected and writes no event.
func TestInvalidSettingWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
		want             error
	}{
		{"unknown key", "colour", "blue", app.ErrUnknownSetting},
		{"invalid model", "model", "gpt-9", app.ErrInvalidSetting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newM2Rig(t)
			if err := r.app.SetSetting(ctx, tc.key, tc.value); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			hist, err := r.app.SettingHistory(ctx, tc.key)
			if err != nil && !errors.Is(err, app.ErrUnknownSetting) {
				t.Fatalf("history: %v", err)
			}
			if len(hist) != 0 {
				t.Errorf("history = %+v, want none", hist)
			}
			if got := r.selector.Current(); got != "default" {
				t.Errorf("model = %q, want default", got)
			}
		})
	}
}

// M2-3. A model chosen through settings is applied by the next process at start.
func TestModelOverrideAppliedAtStart(t *testing.T) {
	ctx := context.Background()
	r := newM2Rig(t)
	if err := r.app.SelectModel(ctx, "sonnet"); err != nil {
		t.Fatalf("select: %v", err)
	}
	r.boot()
	if got := r.selector.Current(); got != "default" {
		t.Fatalf("fresh process model = %q before start, want the config default", got)
	}
	r.start()
	if got := r.selector.Current(); got != "sonnet" {
		t.Errorf("model after start = %q, want sonnet", got)
	}
	if got := r.app.CurrentModel(); got != "sonnet" {
		t.Errorf("CurrentModel = %q, want sonnet", got)
	}
}
