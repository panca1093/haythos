package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panca/haythos/app/gateway"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

const (
	systemTask         = "_system"
	evPaused           = task.EventType("kernel.paused")
	evResumed          = task.EventType("kernel.resumed")
	evJobFired         = task.EventType("job.fired")
	evJobArmed         = task.EventType("job.armed")
	collectCallID      = "collect"
	maxFailReasonBytes = 300
	defaultStepTimeout = 5 * time.Minute
)

type Deps struct {
	Models      map[string]Model
	DefaultJob  Job
	Jobs        []Job
	Tools       Tools
	Events      EventStore
	Ledger      Ledger
	Notifier    Notifier
	Vault       Vault
	Policy      policy.Policy
	Clock       func() time.Time
	ApprovalTTL time.Duration
	StepTimeout time.Duration
	// Hook is called at named crash points; a non-nil error aborts the operation as if the process died.
	Hook func(point string) error

	// ModelSelector is optional; nil disables /model.
	ModelSelector ModelSelector
}

type App struct {
	d      Deps
	gw     *gateway.Gateway
	paused atomic.Bool
	locks  sync.Map
}

type approvalRequested struct {
	CallID    string          `json:"call_id"`
	Approval  policy.Approval `json:"approval"`
	Tier      policy.Tier     `json:"tier"`
	Escalated bool            `json:"escalated"`
	Preview   string          `json:"preview"`
}

type approvalDecided struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
}

type noticeSent struct {
	Key string `json:"key"`
}

func New(d Deps) *App {
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.ApprovalTTL == 0 {
		d.ApprovalTTL = time.Hour
	}
	if d.StepTimeout == 0 {
		d.StepTimeout = defaultStepTimeout
	}
	a := &App{d: d}
	a.gw = gateway.New(gateway.Deps{
		Tools: d.Tools, Ledger: d.Ledger, Vault: d.Vault, Policy: d.Policy,
		Clock: d.Clock, ApprovalTTL: d.ApprovalTTL, Paused: a.paused.Load, Hook: a.hook,
	})
	return a
}

func (a *App) hook(point string) error {
	if a.d.Hook == nil {
		return nil
	}
	return a.d.Hook(point)
}

func (a *App) lock(taskID string) func() {
	m, _ := a.locks.LoadOrStore(taskID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// RestorePause loads the persisted /pause state. Start calls it; one-shot commands that skip Start must too.
func (a *App) RestorePause(ctx context.Context) error {
	sys, err := a.d.Events.Load(ctx, systemTask)
	if err != nil {
		return fmt.Errorf("load system events: %w", err)
	}
	for _, e := range sys {
		switch e.Type {
		case evPaused:
			a.paused.Store(true)
		case evResumed:
			a.paused.Store(false)
		}
	}
	return nil
}

// Start reconciles in-flight ledger rows and resumes unfinished tasks. Call before the scheduler runs.
func (a *App) Start(ctx context.Context) error {
	if err := a.RestorePause(ctx); err != nil {
		return err
	}

	rows, err := a.d.Ledger.InState(ctx, task.LedgerDispatching)
	if err != nil {
		return fmt.Errorf("list dispatching actions: %w", err)
	}
	for _, row := range rows {
		if err := a.reconcile(ctx, row); err != nil {
			return err
		}
	}

	for _, st := range []task.State{task.Pending, task.Running} {
		ids, err := a.d.Events.TasksIn(ctx, st)
		if err != nil {
			return fmt.Errorf("list %s tasks: %w", st, err)
		}
		for _, id := range ids {
			if err := a.RunTask(ctx, id); err != nil {
				return fmt.Errorf("resume task %s: %w", id, err)
			}
		}
	}
	return nil
}

func (a *App) reconcile(ctx context.Context, row task.LedgerEntry) error {
	unlock := a.lock(row.TaskID)
	defer unlock()
	found, err := a.d.Tools.Exists(ctx, row.Tool, row.Target, row.IdemKey)
	unknown := err != nil || !found
	if unknown {
		row.State, row.Result = task.LedgerOutcomeUnknown, "outcome unknown: destination could not confirm the action; it was not retried"
	} else {
		row.State, row.Result = task.LedgerSucceeded, "recovered after restart: destination confirms the action completed"
	}
	row.UpdatedAt = a.d.Clock()
	if err := a.d.Ledger.Put(ctx, row); err != nil {
		return fmt.Errorf("reconcile ledger %s: %w", row.IdemKey, err)
	}
	callID := strings.TrimPrefix(row.IdemKey, row.TaskID+"/")
	s, err := a.Snapshot(ctx, row.TaskID)
	if err != nil {
		return err
	}
	for _, c := range unresolved(s.History) {
		if c.ID == callID {
			if err := a.appendResult(ctx, row.TaskID, task.ToolResult{CallID: callID, Content: row.Result, IsError: unknown}); err != nil {
				return err
			}
		}
	}
	if unknown {
		return a.notify(ctx, row.TaskID, "alert:unknown:"+row.IdemKey, Notice{
			Kind: NoticeAlert,
			Text: fmt.Sprintf("Could not confirm whether %s on %s completed. It was not retried.", row.Tool, row.Target),
		})
	}
	return nil
}

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

func (a *App) CreateTask(ctx context.Context, job Job, input string) (string, error) {
	return a.createTask(ctx, newID(job.Name), job, input)
}

// createTask is idempotent per id so the scheduler can retry after a crash.
func (a *App) createTask(ctx context.Context, id string, job Job, input string) (string, error) {
	existing, err := a.d.Events.Load(ctx, id)
	if err != nil {
		return "", fmt.Errorf("check task %s: %w", id, err)
	}
	if len(existing) > 0 {
		return id, nil
	}
	if job.Model == "" {
		job.Model = a.d.DefaultJob.Model
	}
	steps := []struct {
		typ     task.EventType
		payload any
	}{
		{task.EvCreated, task.Created{Job: job.Name, Model: job.Model, Tools: job.Tools, Budget: job.Budget, Collect: job.Collect, NormalMsg: job.NormalMsg}},
		{task.EvState, task.StateChange{State: task.Pending}},
		{task.EvMessage, task.Message{Role: task.RoleSystem, Text: job.Goal}},
	}
	if input != "" {
		steps = append(steps, struct {
			typ     task.EventType
			payload any
		}{task.EvMessage, task.Message{Role: task.RoleUser, Text: input}})
	}
	for _, s := range steps {
		if _, err := a.d.Events.Append(ctx, id, s.typ, s.payload); err != nil {
			return "", fmt.Errorf("create task %s: %w", id, err)
		}
	}
	return id, nil
}

func (a *App) Snapshot(ctx context.Context, taskID string) (task.Snapshot, error) {
	events, err := a.d.Events.Load(ctx, taskID)
	if err != nil {
		return task.Snapshot{}, fmt.Errorf("load task %s: %w", taskID, err)
	}
	if len(events) == 0 {
		return task.Snapshot{}, fmt.Errorf("task %s: not found", taskID)
	}
	s, err := task.Replay(events)
	if err != nil {
		return task.Snapshot{}, fmt.Errorf("replay task %s: %w", taskID, err)
	}
	return s, nil
}

func (a *App) HandleInput(ctx context.Context, in Input) (string, error) {
	id, err := a.CreateTask(ctx, a.d.DefaultJob, in.Text)
	if err != nil {
		return "", err
	}
	return id, a.RunTask(ctx, id)
}

func (a *App) RunTask(ctx context.Context, taskID string) error {
	unlock := a.lock(taskID)
	defer unlock()
	return a.run(ctx, taskID)
}

func (a *App) run(ctx context.Context, id string) error {
	for {
		s, err := a.Snapshot(ctx, id)
		if err != nil {
			return err
		}
		switch s.State {
		case task.Done, task.Failed, task.WaitingApproval, task.Watching:
			return nil
		case task.Pending:
			if err := a.setState(ctx, id, task.Running, ""); err != nil {
				return err
			}
			continue
		}

		if s.Created.Collect != "" && s.Steps == 0 {
			if err := a.appendMessage(ctx, id, task.Message{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: collectCallID, Name: s.Created.Collect, Args: json.RawMessage(`{}`)}}}); err != nil {
				return err
			}
			continue
		}
		if calls := unresolved(s.History); len(calls) > 0 {
			if err := a.execute(ctx, id, s, calls[0]); err != nil {
				return err
			}
			continue
		}
		if b := s.Created.Budget; (b.Steps > 0 && s.Steps >= b.Steps) || (b.Tokens > 0 && s.Tokens >= b.Tokens) {
			return a.fail(ctx, id, "budget")
		}

		model, ok := a.d.Models[s.Created.Model]
		if !ok {
			return a.fail(ctx, id, fmt.Sprintf("model %q is not configured", s.Created.Model))
		}
		stepCtx, cancel := context.WithTimeout(ctx, a.d.StepTimeout)
		turn, err := model.Step(stepCtx, s.History, a.d.Tools.List(s.Created.Tools))
		timedOut := errors.Is(stepCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return ctx.Err()
			case timedOut:
				return a.fail(ctx, id, fmt.Sprintf("model step timed out after %s", a.d.StepTimeout))
			}
			return a.fail(ctx, id, truncate(err.Error(), maxFailReasonBytes))
		}
		if _, err := a.d.Events.Append(ctx, id, task.EvUsage, turn.Usage); err != nil {
			return fmt.Errorf("append usage: %w", err)
		}
		if err := a.appendMessage(ctx, id, task.Message{Role: task.RoleAssistant, Text: turn.Text, Calls: turn.Calls}); err != nil {
			return err
		}
		if len(turn.Calls) == 0 {
			return a.finish(ctx, id, turn.Text)
		}
	}
}

func (a *App) execute(ctx context.Context, id string, s task.Snapshot, call task.ToolCall) error {
	events, err := a.d.Events.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("load task %s: %w", id, err)
	}
	out, err := a.gw.Execute(ctx, gateway.Request{
		TaskID: id, Call: call, Allowlist: s.Created.Tools,
		Tainted: s.Tainted, Trusted: s.Trusted, Approval: grantedApproval(events, call.ID),
	})
	switch {
	case errors.Is(err, policy.ErrApprovalVoid):
		if nerr := a.notify(ctx, id, "alert:void:"+call.ID, Notice{Kind: NoticeAlert, Text: "An approval was voided: " + err.Error()}); nerr != nil {
			return nerr
		}
		return a.appendResult(ctx, id, task.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true})
	case err != nil:
		return err
	case out.Pending != nil:
		return a.requestApproval(ctx, id, call, out.Pending)
	}
	if err := a.appendResult(ctx, id, out.Result); err != nil {
		return err
	}
	if out.Unknown {
		if err := a.notify(ctx, id, "alert:unknown:"+id+"/"+call.ID, Notice{
			Kind: NoticeAlert, Text: fmt.Sprintf("Could not tell whether %s completed. It will not be retried; please check.", call.Name),
		}); err != nil {
			return err
		}
	}
	if out.Result.Normal && s.Created.NormalMsg != "" {
		if err := a.appendMessage(ctx, id, task.Message{Role: task.RoleAssistant, Text: s.Created.NormalMsg}); err != nil {
			return err
		}
		return a.finish(ctx, id, s.Created.NormalMsg)
	}
	return nil
}

func (a *App) requestApproval(ctx context.Context, id string, call task.ToolCall, p *gateway.Pending) error {
	rec := approvalRequested{CallID: call.ID, Approval: p.Approval, Tier: p.Tier, Escalated: p.Escalated, Preview: p.Preview}
	if _, err := a.d.Events.Append(ctx, id, task.EvApprovalRequested, rec); err != nil {
		return fmt.Errorf("append approval request: %w", err)
	}
	if err := a.setState(ctx, id, task.WaitingApproval, "approval "+p.Approval.ID); err != nil {
		return err
	}
	return a.notifyApproval(ctx, id, rec)
}

func (a *App) notifyApproval(ctx context.Context, id string, rec approvalRequested) error {
	return a.notify(ctx, id, "approval:"+rec.Approval.ID, Notice{
		Kind: NoticeApproval, Text: rec.Preview,
		Approval: &ApprovalRequest{Approval: rec.Approval, Tier: rec.Tier, Escalated: rec.Escalated, Preview: rec.Preview},
	})
}

func (a *App) HandleApproval(ctx context.Context, taskID, approvalID string, approve bool) error {
	unlock := a.lock(taskID)
	defer unlock()
	events, err := a.d.Events.Load(ctx, taskID)
	if err != nil {
		return fmt.Errorf("load task %s: %w", taskID, err)
	}
	s, err := a.Snapshot(ctx, taskID)
	if err != nil {
		return err
	}
	if s.State != task.WaitingApproval {
		return fmt.Errorf("task %s is %s, not waiting for approval", taskID, s.State)
	}
	req, ok := findRequest(events, approvalID)
	if !ok {
		return fmt.Errorf("approval %s not found on task %s", approvalID, taskID)
	}
	if _, err := a.d.Events.Append(ctx, taskID, task.EvApprovalDecided, approvalDecided{ID: approvalID, Approved: approve}); err != nil {
		return fmt.Errorf("append approval decision: %w", err)
	}
	if err := a.setState(ctx, taskID, task.Running, "approval decided"); err != nil {
		return err
	}
	if !approve {
		if err := a.appendResult(ctx, taskID, task.ToolResult{CallID: req.CallID, Content: "rejected by the user", IsError: true}); err != nil {
			return err
		}
	}
	return a.run(ctx, taskID)
}

func (a *App) finish(ctx context.Context, id, text string) error {
	if err := a.setState(ctx, id, task.Done, ""); err != nil {
		return err
	}
	return a.notify(ctx, id, "final", Notice{Kind: NoticeMessage, Text: text})
}

func (a *App) fail(ctx context.Context, id, reason string) error {
	if err := a.setState(ctx, id, task.Failed, reason); err != nil {
		return err
	}
	return a.notify(ctx, id, "failed", Notice{Kind: NoticeState, State: task.Failed, Reason: reason, Text: "Task failed: " + reason})
}

func (a *App) setState(ctx context.Context, id string, st task.State, reason string) error {
	if _, err := a.d.Events.Append(ctx, id, task.EvState, task.StateChange{State: st, Reason: reason}); err != nil {
		return fmt.Errorf("append state %s: %w", st, err)
	}
	return nil
}

func (a *App) appendMessage(ctx context.Context, id string, m task.Message) error {
	if _, err := a.d.Events.Append(ctx, id, task.EvMessage, m); err != nil {
		return fmt.Errorf("append message: %w", err)
	}
	return nil
}

func (a *App) appendResult(ctx context.Context, id string, r task.ToolResult) error {
	if err := a.appendMessage(ctx, id, task.Message{Role: task.RoleTool, Results: []task.ToolResult{r}}); err != nil {
		return err
	}
	return a.hook("app.after_tool_result")
}

// notify publishes at most once per key. A crash between Publish and the record can repeat one notice.
// TODO: uncertain — at-least-once window between Publish and notice.sent append, verify before prod
func (a *App) notify(ctx context.Context, id, key string, n Notice) error {
	events, err := a.d.Events.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("load task %s: %w", id, err)
	}
	for _, e := range events {
		var sent noticeSent
		if e.Type == task.EvNoticeSent && json.Unmarshal(e.Payload, &sent) == nil && sent.Key == key {
			return nil
		}
	}
	n.Key, n.TaskID = key, id
	if err := a.d.Notifier.Publish(ctx, n); err != nil {
		slog.Warn("publish notice failed", "task", id, "key", key, "err", err)
		return nil
	}
	if _, err := a.d.Events.Append(ctx, id, task.EvNoticeSent, noticeSent{Key: key}); err != nil {
		return fmt.Errorf("record notice %s: %w", key, err)
	}
	return nil
}

func (a *App) Pause(ctx context.Context) error {
	a.paused.Store(true)
	if _, err := a.d.Events.Append(ctx, systemTask, evPaused, struct{}{}); err != nil {
		return fmt.Errorf("record pause: %w", err)
	}
	return nil
}

func (a *App) Resume(ctx context.Context) error {
	if _, err := a.d.Events.Append(ctx, systemTask, evResumed, struct{}{}); err != nil {
		return fmt.Errorf("record resume: %w", err)
	}
	a.paused.Store(false)
	return nil
}

func unresolved(history []task.Message) []task.ToolCall {
	last := -1
	for i, m := range history {
		if m.Role == task.RoleAssistant {
			last = i
		}
	}
	if last < 0 || len(history[last].Calls) == 0 {
		return nil
	}
	done := map[string]bool{}
	for _, m := range history[last+1:] {
		for _, r := range m.Results {
			done[r.CallID] = true
		}
	}
	var out []task.ToolCall
	for _, c := range history[last].Calls {
		if !done[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

func findRequest(events []task.Event, approvalID string) (approvalRequested, bool) {
	for _, e := range events {
		var r approvalRequested
		if e.Type == task.EvApprovalRequested && json.Unmarshal(e.Payload, &r) == nil && r.Approval.ID == approvalID {
			return r, true
		}
	}
	return approvalRequested{}, false
}

func grantedApproval(events []task.Event, callID string) *policy.Approval {
	var req *approvalRequested
	for _, e := range events {
		var r approvalRequested
		if e.Type == task.EvApprovalRequested && json.Unmarshal(e.Payload, &r) == nil && r.CallID == callID {
			req = &r
		}
	}
	if req == nil {
		return nil
	}
	for _, e := range events {
		var d approvalDecided
		if e.Type == task.EvApprovalDecided && json.Unmarshal(e.Payload, &d) == nil && d.ID == req.Approval.ID && d.Approved {
			return &req.Approval
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (a *App) RunJob(ctx context.Context, name string) (string, error) {
	for _, j := range a.d.Jobs {
		if j.Name == name {
			id, err := a.CreateTask(ctx, j, "Run the scheduled job: "+j.Name)
			if err != nil {
				return "", err
			}
			return id, a.RunTask(ctx, id)
		}
	}
	return "", fmt.Errorf("job %q not found", name)
}
