package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func jobsApp(t *testing.T) (*app.App, *testfakes.Events) {
	t.Helper()
	ev := &testfakes.Events{}
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(), "fast": testfakes.NewModel()},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred."},
		Jobs:       []app.Job{{Name: "vm-health"}},
		Tools:      testfakes.NewTools(task.ToolSpec{Name: "vm.read"}, task.ToolSpec{Name: "note.add", Write: true}),
		Events:     ev,
		Policy:     policy.Policy{Tiers: map[string]policy.Tier{"vm.read": policy.TierRead, "note.add": policy.TierInternalWrite}},
		Clock:      func() time.Time { return time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC) },
	}), ev
}

func TestDefineJobValidation(t *testing.T) {
	ok := app.JobDef{Name: "disk", Cron: "0 9 * * 1", Goal: "check", Tools: []string{"vm.read"}}
	tests := []struct {
		name    string
		mutate  func(*app.JobDef)
		wantErr string
	}{
		{name: "valid"},
		{name: "tier 1 tool allowed", mutate: func(d *app.JobDef) { d.Tools = []string{"note.add"} }},
		{name: "configured model", mutate: func(d *app.JobDef) { d.Model = "fast" }},
		{name: "explicit timezone kept", mutate: func(d *app.JobDef) { d.Cron = "CRON_TZ=UTC 0 9 * * 1" }},
		{name: "chat name reserved", mutate: func(d *app.JobDef) { d.Name = "chat" }, wantErr: "config job"},
		{name: "empty goal", mutate: func(d *app.JobDef) { d.Goal = " " }, wantErr: "goal is empty"},
		{name: "unknown model", mutate: func(d *app.JobDef) { d.Model = "gpt-9" }, wantErr: "not configured"},
		{name: "tool in policy but not registered", mutate: func(d *app.JobDef) { d.Tools = []string{"ghost"} }, wantErr: "does not exist"},
		{name: "budget over cap", mutate: func(d *app.JobDef) { d.Budget = task.Budget{Tokens: 1000001, Steps: 5} }, wantErr: "budget"},
		{name: "partial budget", mutate: func(d *app.JobDef) { d.Budget = task.Budget{Tokens: 1000} }, wantErr: "budget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, _ := jobsApp(t)
			def := ok
			if tt.mutate != nil {
				tt.mutate(&def)
			}
			err := a.DefineJob(context.Background(), def)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if !errors.Is(err, app.ErrInvalidJob) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want ErrInvalidJob containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestDefineJobDefaultsTimezoneAndBudget(t *testing.T) {
	a, _ := jobsApp(t)
	if err := a.DefineJob(context.Background(), app.JobDef{Name: "disk", Cron: "0 9 * * 1", Goal: "check"}); err != nil {
		t.Fatal(err)
	}
	js, err := a.ListJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := js[len(js)-1]
	if got.Cron != "CRON_TZ=Asia/Jakarta 0 9 * * 1" || got.Source != "chat" || !got.Enabled {
		t.Errorf("job = %+v", got)
	}
	if want := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC); !got.Next.Equal(want) {
		t.Errorf("next = %s, want %s", got.Next.UTC(), want)
	}
}

func TestRedefineReplacesAndKeepsDisabled(t *testing.T) {
	ctx := context.Background()
	a, _ := jobsApp(t)
	def := app.JobDef{Name: "disk", Cron: "0 9 * * 1", Goal: "check"}
	if err := a.DefineJob(ctx, def); err != nil {
		t.Fatal(err)
	}
	if err := a.SetJobEnabled(ctx, "disk", false); err != nil {
		t.Fatal(err)
	}
	def.Cron = "0 10 * * 1"
	if err := a.DefineJob(ctx, def); err != nil {
		t.Fatal(err)
	}
	js, _ := a.ListJobs(ctx)
	if len(js) != 2 || js[1].Cron != "CRON_TZ=Asia/Jakarta 0 10 * * 1" || js[1].Enabled {
		t.Errorf("jobs = %+v", js)
	}
}

func TestOnlyChatJobsCanBeChanged(t *testing.T) {
	ctx := context.Background()
	a, _ := jobsApp(t)
	if err := a.SetJobEnabled(ctx, "vm-health", false); !errors.Is(err, app.ErrUnknownJob) {
		t.Errorf("disable config job err = %v", err)
	}
	if err := a.DeleteJob(ctx, "nope"); !errors.Is(err, app.ErrUnknownJob) {
		t.Errorf("delete unknown err = %v", err)
	}
}

func TestChatJobCannotUseTierTwoWrites(t *testing.T) {
	t.Parallel()
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel()},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred."},
		Tools:      testfakes.NewTools(task.ToolSpec{Name: "discord.post", Write: true, TargetArg: "channel"}),
		Events:     &testfakes.Events{},
		Policy:     policy.Policy{Tiers: map[string]policy.Tier{"discord.post": policy.TierExternalWrite}},
		Clock:      func() time.Time { return time.Date(2026, 10, 27, 2, 0, 0, 0, time.UTC) },
	})
	err := a.DefineJob(context.Background(), app.JobDef{Name: "poster", Cron: "0 9 * * 1", Goal: "post", Tools: []string{"discord.post"}})
	if !errors.Is(err, app.ErrInvalidJob) || !strings.Contains(err.Error(), "tier 2") {
		t.Errorf("err = %v, want ErrInvalidJob mentioning tier 2", err)
	}
}
