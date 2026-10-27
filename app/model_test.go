package app_test

import (
	"context"
	"errors"
	"testing"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/internal/testfakes"
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
				if e.TaskID != "_system" || e.Type != "kernel.model_selected" {
					t.Errorf("event = %s %s", e.TaskID, e.Type)
				}
				if string(e.Payload) != `{"model":"sonnet"}` {
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
