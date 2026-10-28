package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/jobs"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type fakeKernel struct {
	jobs      []app.JobStatus
	defineErr error
	defined   []app.JobDef
}

func (k *fakeKernel) DefineJob(_ context.Context, def app.JobDef) error {
	if k.defineErr != nil {
		return k.defineErr
	}
	k.defined = append(k.defined, def)
	return nil
}

func (k *fakeKernel) SetJobEnabled(context.Context, string, bool) error { return nil }
func (k *fakeKernel) DeleteJob(context.Context, string) error           { return nil }
func (k *fakeKernel) ListJobs(context.Context) ([]app.JobStatus, error) { return k.jobs, nil }

func TestDefineReportsKernelErrorsToTheModel(t *testing.T) {
	k := &fakeKernel{defineErr: errors.New("invalid job: cron \"x\"")}
	r := local.New()
	jobs.Register(r, k)
	res, err := r.Call(context.Background(), task.ToolCall{Name: "jobs.define", Args: json.RawMessage(`{"name":"a","cron":"x","goal":"g","tools":[]}`)})
	if err != nil || !res.IsError || res.Content != `invalid job: cron "x"` {
		t.Errorf("result = %+v, err = %v", res, err)
	}
}

func TestDefinePassesTheJob(t *testing.T) {
	k := &fakeKernel{}
	r := local.New()
	jobs.Register(r, k)
	res, err := r.Call(context.Background(), task.ToolCall{Name: "jobs.define", Args: json.RawMessage(`{"name":"disk","cron":"0 9 * * 1","goal":"check","tools":["vm.read"],"budget":{"tokens":100,"steps":2}}`)})
	if err != nil || res.IsError || len(k.defined) != 1 {
		t.Fatalf("result = %+v, err = %v, defined = %+v", res, err, k.defined)
	}
	if d := k.defined[0]; d.Name != "disk" || d.Budget != (task.Budget{Tokens: 100, Steps: 2}) || d.Tools[0] != "vm.read" {
		t.Errorf("def = %+v", d)
	}
}

func TestWriteToolsAreTargetedByName(t *testing.T) {
	r := local.New()
	jobs.Register(r, &fakeKernel{})
	for _, s := range r.List([]string{"jobs.list", "jobs.define", "jobs.disable", "jobs.enable", "jobs.delete"}) {
		if wantWrite := s.Name != "jobs.list"; s.Write != wantWrite || (wantWrite && s.TargetArg != "name") {
			t.Errorf("%s: write = %v target = %q", s.Name, s.Write, s.TargetArg)
		}
	}
}

func TestExistsMatchesTheIntendedEffect(t *testing.T) {
	k := &fakeKernel{jobs: []app.JobStatus{
		{Name: "vm-health", Source: "config", Enabled: true},
		{Name: "on", Source: "chat", Enabled: true},
		{Name: "off", Source: "chat", Enabled: false},
	}}
	r := local.New()
	jobs.Register(r, k)
	tests := []struct {
		tool, target string
		want         bool
	}{
		{"jobs.define", "on", true},
		{"jobs.define", "missing", false},
		{"jobs.define", "vm-health", false},
		{"jobs.disable", "off", true},
		{"jobs.disable", "on", false},
		{"jobs.enable", "on", true},
		{"jobs.enable", "off", false},
		{"jobs.delete", "missing", true},
		{"jobs.delete", "on", false},
	}
	for _, tt := range tests {
		t.Run(tt.tool+" "+tt.target, func(t *testing.T) {
			got, err := r.Exists(context.Background(), tt.tool, tt.target, "k")
			if err != nil || got != tt.want {
				t.Errorf("exists = %v, %v, want %v", got, err, tt.want)
			}
		})
	}
	if _, err := r.Exists(context.Background(), "vmhealth.read", "", "k"); !errors.Is(err, app.ErrCannotVerify) {
		t.Errorf("tool without a check: err = %v, want ErrCannotVerify", err)
	}
}

func TestDefineAdvertisesAndPassesDeliverTo(t *testing.T) {
	k := &fakeKernel{}
	r := local.New()
	jobs.Register(r, k)
	spec := r.List([]string{"jobs.define"})[0]
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Properties["deliver_to"].Type != "array" || slices.Contains(schema.Required, "deliver_to") {
		t.Errorf("deliver_to must be an optional array, schema = %s", spec.InputSchema)
	}
	res, err := r.Call(context.Background(), task.ToolCall{Name: "jobs.define", Args: json.RawMessage(`{"name":"disk","cron":"0 9 * * 1","goal":"check","tools":[],"deliver_to":["123","dm"]}`)})
	if err != nil || res.IsError || len(k.defined) != 1 || !slices.Equal(k.defined[0].DeliverTo, []string{"123", "dm"}) {
		t.Errorf("result = %+v, err = %v, defined = %+v", res, err, k.defined)
	}
}
