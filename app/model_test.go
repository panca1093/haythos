package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
)

var liveModels = []app.ModelChoice{
	{Value: "default", Name: "Default", Description: "Opus"},
	{Value: "sonnet", Name: "Sonnet"},
}

func TestSelectModel(t *testing.T) {
	boom := errors.New("sdk down")
	tests := []struct {
		name       string
		noSelector bool
		sel        *testfakes.ModelSelector
		value      string
		wantErr    error
		wantCur    string
		wantEvents int
	}{
		{name: "valid", sel: &testfakes.ModelSelector{Choices: liveModels, Cur: "default"}, value: "sonnet", wantCur: "sonnet", wantEvents: 1},
		{name: "unknown value", sel: &testfakes.ModelSelector{Choices: liveModels, Cur: "default"}, value: "sonet", wantErr: app.ErrUnknownModel, wantCur: "default"},
		{name: "empty value", sel: &testfakes.ModelSelector{Choices: liveModels, Cur: "default"}, value: "", wantErr: app.ErrUnknownModel, wantCur: "default"},
		{name: "no selector", noSelector: true, value: "sonnet", wantErr: app.ErrNoModelSelector},
		{name: "list fails", sel: &testfakes.ModelSelector{ListErr: boom, Cur: "default"}, value: "sonnet", wantErr: boom, wantCur: "default"},
		{name: "select fails records nothing", sel: &testfakes.ModelSelector{Choices: liveModels, Cur: "default", SelectErr: boom}, value: "sonnet", wantErr: boom, wantCur: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ev := &testfakes.Events{}
			d := app.Deps{Events: ev}
			if !tt.noSelector {
				d.ModelSelector = tt.sel
			}
			a := app.New(d)
			err := a.SelectModel(context.Background(), tt.value)
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !tt.noSelector && tt.sel.Cur != tt.wantCur {
				t.Errorf("current = %q, want %q", tt.sel.Cur, tt.wantCur)
			}
			if len(ev.Appended) != tt.wantEvents {
				t.Fatalf("events = %d, want %d", len(ev.Appended), tt.wantEvents)
			}
			if tt.wantEvents == 1 {
				e := ev.Appended[0]
				if e.TaskID != "_system" || e.Type != "setting.changed" {
					t.Errorf("event = %s %s", e.TaskID, e.Type)
				}
				if string(e.Payload) != `{"key":"model","value":"sonnet"}` {
					t.Errorf("payload = %s", e.Payload)
				}
			}
		})
	}
}

func TestSelectModelAllowedWhilePaused(t *testing.T) {
	sel := &testfakes.ModelSelector{Choices: liveModels, Cur: "default"}
	a := app.New(app.Deps{Events: &testfakes.Events{}, ModelSelector: sel})
	if err := a.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.SelectModel(context.Background(), "sonnet"); err != nil || sel.Cur != "sonnet" {
		t.Errorf("err = %v, current = %q", err, sel.Cur)
	}
}

func TestSelectModelEventFailureIsReported(t *testing.T) {
	ev := &testfakes.Events{AppendErr: errors.New("disk full")}
	a := app.New(app.Deps{Events: ev, ModelSelector: &testfakes.ModelSelector{Choices: liveModels}})
	if err := a.SelectModel(context.Background(), "sonnet"); err == nil {
		t.Fatal("want error when the audit event cannot be written")
	}
}

func TestListAndCurrentModel(t *testing.T) {
	a := app.New(app.Deps{ModelSelector: &testfakes.ModelSelector{Choices: liveModels, Cur: "sonnet"}})
	got, err := a.ListModels(context.Background())
	if err != nil || len(got) != 2 || got[0].Value != "default" {
		t.Errorf("list = %v, %v", got, err)
	}
	if cur := a.CurrentModel(); cur != "sonnet" {
		t.Errorf("current = %q", cur)
	}
	none := app.New(app.Deps{})
	if _, err := none.ListModels(context.Background()); !errors.Is(err, app.ErrNoModelSelector) {
		t.Errorf("list err = %v", err)
	}
	if cur := none.CurrentModel(); cur != "" {
		t.Errorf("current without selector = %q", cur)
	}
}

type waitingCatalog struct {
	app.Model
	started  chan struct{}
	released chan struct{}
}

func (m waitingCatalog) List(context.Context) ([]app.ModelChoice, error) {
	close(m.started)
	<-m.released
	return nil, nil
}

func (m waitingCatalog) WithModel(string) app.Model { return m.Model }

func TestChatModelsReturnsAvailableChoicesWhenDiscoveryIsCanceled(t *testing.T) {
	model := waitingCatalog{started: make(chan struct{}), released: make(chan struct{})}
	defer close(model.released)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := app.New(app.Deps{Models: map[string]app.Model{"subscription": model}})
	type result struct {
		choices []app.ModelChoice
		err     error
	}
	done := make(chan result, 1)
	go func() {
		choices, err := a.ChatModels(ctx)
		done <- result{choices: choices, err: err}
	}()
	<-model.started
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) || len(got.choices) != 1 || got.choices[0].Value != "subscription" {
			t.Fatalf("choices = %v, err = %v", got.choices, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("model list waited for a provider after cancellation")
	}
}
