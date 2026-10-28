package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type Kernel interface {
	DefineJob(ctx context.Context, def app.JobDef) error
	SetJobEnabled(ctx context.Context, name string, enabled bool) error
	DeleteJob(ctx context.Context, name string) error
	ListJobs(ctx context.Context) ([]app.JobStatus, error)
}

const nameSchema = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`

const defineSchema = `{"type":"object","properties":{
"name":{"type":"string","description":"lowercase slug, e.g. disk-check"},
"cron":{"type":"string","description":"5-field cron; Asia/Jakarta unless CRON_TZ= is given"},
"goal":{"type":"string","description":"what the job must check or report"},
"tools":{"type":"array","items":{"type":"string"}},
"model":{"type":"string"},
"budget":{"type":"object","properties":{"tokens":{"type":"integer"},"steps":{"type":"integer"}}}
},"required":["name","cron","goal","tools"]}`

// Register adds the jobs.* tools; they change only the kernel's own schedule.
func Register(r *local.Registry, k Kernel) {
	r.Register(task.ToolSpec{
		Name: "jobs.list", Description: "List scheduled jobs with source, cron, tools, enabled state and next run.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, _ []byte) (task.ToolResult, error) {
		js, err := k.ListJobs(ctx)
		if err != nil {
			return task.ToolResult{}, err
		}
		b, err := json.Marshal(js)
		if err != nil {
			return task.ToolResult{}, fmt.Errorf("encode jobs: %w", err)
		}
		return task.ToolResult{Content: string(b)}, nil
	})

	r.RegisterWrite(task.ToolSpec{
		Name: "jobs.define", Description: "Create or replace a scheduled job. Tools must be read or internal tools.",
		Write: true, TargetArg: "name", InputSchema: json.RawMessage(defineSchema),
	}, func(ctx context.Context, args []byte) (task.ToolResult, error) {
		var def app.JobDef
		if err := json.Unmarshal(args, &def); err != nil {
			return errResult(fmt.Errorf("decode job: %w", err)), nil
		}
		if err := k.DefineJob(ctx, def); err != nil {
			return errResult(err), nil
		}
		return task.ToolResult{Content: "job " + def.Name + " is scheduled"}, nil
	}, state(k, func(j *app.JobStatus) bool { return j != nil }))

	for _, t := range []struct {
		name, desc string
		do         func(ctx context.Context, name string) error
		done       func(*app.JobStatus) bool
	}{
		{"jobs.disable", "Pause a job created from chat.", func(ctx context.Context, n string) error { return k.SetJobEnabled(ctx, n, false) },
			func(j *app.JobStatus) bool { return j != nil && !j.Enabled }},
		{"jobs.enable", "Resume a disabled job; missed runs are not replayed.", func(ctx context.Context, n string) error { return k.SetJobEnabled(ctx, n, true) },
			func(j *app.JobStatus) bool { return j != nil && j.Enabled }},
		{"jobs.delete", "Delete a job created from chat.", k.DeleteJob,
			func(j *app.JobStatus) bool { return j == nil }},
	} {
		r.RegisterWrite(task.ToolSpec{
			Name: t.name, Description: t.desc, Write: true, TargetArg: "name", InputSchema: json.RawMessage(nameSchema),
		}, func(ctx context.Context, args []byte) (task.ToolResult, error) {
			var n struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(args, &n); err != nil {
				return errResult(fmt.Errorf("decode args: %w", err)), nil
			}
			if err := t.do(ctx, n.Name); err != nil {
				return errResult(err), nil
			}
			return task.ToolResult{Content: t.name + " " + n.Name + ": done"}, nil
		}, state(k, t.done))
	}
}

func errResult(err error) task.ToolResult {
	return task.ToolResult{Content: err.Error(), IsError: true}
}

// state checks the job's current state, so a crash between dispatch and success is recovered without a retry.
func state(k Kernel, done func(*app.JobStatus) bool) local.ExistsFunc {
	return func(ctx context.Context, name string) (bool, error) {
		js, err := k.ListJobs(ctx)
		if err != nil {
			return false, err
		}
		i := slices.IndexFunc(js, func(j app.JobStatus) bool { return j.Source == "chat" && j.Name == name })
		if i < 0 {
			return done(nil), nil
		}
		return done(&js[i]), nil
	}
}
