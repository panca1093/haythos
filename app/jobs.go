package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

const (
	evJobDefined  = task.EventType("job.defined")
	evJobDisabled = task.EventType("job.disabled")
	evJobEnabled  = task.EventType("job.enabled")
	evJobDeleted  = task.EventType("job.deleted")

	defaultCronTZ  = "Asia/Jakarta"
	maxJobTokens   = 1000000
	maxJobSteps    = 10
	kernelToolsPfx = "jobs."
)

var (
	ErrInvalidJob = errors.New("invalid job")
	ErrUnknownJob = errors.New("unknown job")

	jobNameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	defaultBudget = task.Budget{Tokens: 200000, Steps: 5}
)

// JobDef is a job created at runtime from chat; YAML jobs never become one.
type JobDef struct {
	Name   string      `json:"name"`
	Cron   string      `json:"cron"`
	Goal   string      `json:"goal"`
	Tools  []string    `json:"tools"`
	Model  string      `json:"model,omitempty"`
	Budget task.Budget `json:"budget,omitempty"`

	DeliverTo []string `json:"deliver_to,omitempty"`
}

type JobStatus struct {
	Name      string
	Source    string
	Cron      string
	Tools     []string
	DeliverTo []string
	Enabled   bool
	Next      time.Time
}

type jobName struct {
	Name string `json:"name"`
}

type chatJob struct {
	def     JobDef
	enabled bool
}

func (a *App) DefineJob(ctx context.Context, def JobDef) error {
	def.DeliverTo = cleanTargets(def.DeliverTo)
	if len(def.DeliverTo) == 0 {
		if origin := originFrom(ctx); origin != "" {
			def.DeliverTo = []string{origin}
		}
	}
	def, err := a.validJob(def)
	if err != nil {
		return err
	}
	if _, err := a.d.Events.Append(ctx, systemTask, evJobDefined, def); err != nil {
		return fmt.Errorf("record job %s: %w", def.Name, err)
	}
	return a.armJob(ctx, def.Name)
}

func (a *App) SetJobEnabled(ctx context.Context, name string, enabled bool) error {
	if err := a.requireChatJob(ctx, name); err != nil {
		return err
	}
	typ := evJobDisabled
	if enabled {
		typ = evJobEnabled
	}
	if _, err := a.d.Events.Append(ctx, systemTask, typ, jobName{Name: name}); err != nil {
		return fmt.Errorf("record %s %s: %w", typ, name, err)
	}
	if enabled {
		return a.armJob(ctx, name)
	}
	return nil
}

func (a *App) DeleteJob(ctx context.Context, name string) error {
	if err := a.requireChatJob(ctx, name); err != nil {
		return err
	}
	if _, err := a.d.Events.Append(ctx, systemTask, evJobDeleted, jobName{Name: name}); err != nil {
		return fmt.Errorf("record delete %s: %w", name, err)
	}
	return nil
}

func (a *App) ListJobs(ctx context.Context) ([]JobStatus, error) {
	sys, err := a.d.Events.Load(ctx, systemTask)
	if err != nil {
		return nil, fmt.Errorf("load system events: %w", err)
	}
	chat, err := chatJobs(sys)
	if err != nil {
		return nil, err
	}
	now := a.d.Clock()
	var out []JobStatus
	for _, j := range a.d.Jobs {
		out = append(out, JobStatus{Name: j.Name, Source: "config", Cron: j.Trigger.Cron, Tools: j.Tools, DeliverTo: j.DeliverTo, Enabled: true, Next: nextRun(j.Trigger.Cron, now)})
	}
	for _, c := range chat {
		st := JobStatus{Name: c.def.Name, Source: "chat", Cron: c.def.Cron, Tools: c.def.Tools, DeliverTo: c.def.DeliverTo, Enabled: c.enabled}
		if c.enabled {
			st.Next = nextRun(c.def.Cron, now)
		}
		out = append(out, st)
	}
	return out, nil
}

// scheduledJobs is every job Tick may fire: YAML jobs plus enabled chat jobs.
func (a *App) scheduledJobs(sys []task.Event) ([]Job, error) {
	chat, err := chatJobs(sys)
	if err != nil {
		return nil, err
	}
	out := append([]Job(nil), a.d.Jobs...)
	for _, c := range chat {
		if c.enabled {
			out = append(out, a.jobFrom(c.def))
		}
	}
	return out, nil
}

func (a *App) jobFrom(def JobDef) Job {
	return Job{
		Name: def.Name, Model: def.Model, Tools: def.Tools, Budget: def.Budget,
		Goal:    a.d.DefaultJob.Goal + "\n\nScheduled job goal: " + def.Goal,
		Trigger: Trigger{Cron: def.Cron}, DeliverTo: def.DeliverTo,
	}
}

func cleanTargets(in []string) []string {
	var out []string
	for _, t := range in {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func (a *App) validJob(def JobDef) (JobDef, error) {
	bad := func(format string, args ...any) (JobDef, error) {
		return JobDef{}, fmt.Errorf("%w: %s", ErrInvalidJob, fmt.Sprintf(format, args...))
	}
	if !jobNameRe.MatchString(def.Name) {
		return bad("name %q must be lowercase letters, digits and dashes, at most 40 characters", def.Name)
	}
	if def.Name == a.d.DefaultJob.Name || slices.ContainsFunc(a.d.Jobs, func(j Job) bool { return j.Name == def.Name }) {
		return bad("name %q belongs to a config job", def.Name)
	}
	if strings.TrimSpace(def.Goal) == "" {
		return bad("goal is empty")
	}
	if !strings.HasPrefix(def.Cron, "CRON_TZ=") && !strings.HasPrefix(def.Cron, "TZ=") {
		def.Cron = "CRON_TZ=" + defaultCronTZ + " " + def.Cron
	}
	if _, err := cron.ParseStandard(def.Cron); err != nil {
		return bad("cron %q: %v", def.Cron, err)
	}
	for _, t := range def.Tools {
		tier, ok := a.d.Policy.Tiers[t]
		switch {
		case strings.HasPrefix(t, kernelToolsPfx):
			return bad("tool %s cannot be used inside a job", t)
		case !ok || len(a.d.Tools.List([]string{t})) == 0:
			return bad("tool %s does not exist", t)
		case tier > policy.TierInternalWrite:
			return bad("tool %s is tier %d; background jobs allow tier 0 or 1", t, tier)
		}
	}
	if def.Model != "" {
		if _, ok := a.d.Models[def.Model]; !ok {
			return bad("model %q is not configured", def.Model)
		}
	}
	if def.Budget == (task.Budget{}) {
		def.Budget = defaultBudget
	}
	if def.Budget.Tokens <= 0 || def.Budget.Tokens > maxJobTokens || def.Budget.Steps <= 0 || def.Budget.Steps > maxJobSteps {
		return bad("budget must be 1..%d tokens and 1..%d steps", maxJobTokens, maxJobSteps)
	}
	return def, nil
}

func (a *App) requireChatJob(ctx context.Context, name string) error {
	sys, err := a.d.Events.Load(ctx, systemTask)
	if err != nil {
		return fmt.Errorf("load system events: %w", err)
	}
	chat, err := chatJobs(sys)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(chat, func(c chatJob) bool { return c.def.Name == name }) {
		return fmt.Errorf("%w: %q is not a job created from chat", ErrUnknownJob, name)
	}
	return nil
}

// armJob starts the schedule now, so runs missed before definition or while disabled are not replayed.
func (a *App) armJob(ctx context.Context, name string) error {
	if _, err := a.d.Events.Append(ctx, systemTask, evJobArmed, jobMark{Job: name, At: a.d.Clock()}); err != nil {
		return fmt.Errorf("arm job %s: %w", name, err)
	}
	return nil
}

// chatJobs replays job events in definition order.
func chatJobs(sys []task.Event) ([]chatJob, error) {
	var out []chatJob
	find := func(name string) int {
		return slices.IndexFunc(out, func(c chatJob) bool { return c.def.Name == name })
	}
	for _, e := range sys {
		switch e.Type {
		case evJobDefined:
			var def JobDef
			if err := json.Unmarshal(e.Payload, &def); err != nil {
				return nil, fmt.Errorf("decode job event %d: %w", e.Cursor, err)
			}
			if i := find(def.Name); i >= 0 {
				out[i].def = def
			} else {
				out = append(out, chatJob{def: def, enabled: true})
			}
		case evJobDisabled, evJobEnabled, evJobDeleted:
			var n jobName
			if err := json.Unmarshal(e.Payload, &n); err != nil {
				return nil, fmt.Errorf("decode job event %d: %w", e.Cursor, err)
			}
			i := find(n.Name)
			if i < 0 {
				continue
			}
			switch e.Type {
			case evJobDeleted:
				out = slices.Delete(out, i, i+1)
			default:
				out[i].enabled = e.Type == evJobEnabled
			}
		}
	}
	return out, nil
}

func nextRun(spec string, now time.Time) time.Time {
	if spec == "" {
		return time.Time{}
	}
	s, err := cron.ParseStandard(spec)
	if err != nil {
		return time.Time{}
	}
	return s.Next(now)
}
