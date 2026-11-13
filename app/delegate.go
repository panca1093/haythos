package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/panca/haythos/core/contract"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

var ErrDelegated = errors.New("task is delegated to a runtime")

const (
	cancelGrace    = 10 * time.Second
	maxCheckOutput = 8 << 10
)

// Check is a command the kernel runs after the session; Argv is never a shell string.
type Check struct {
	Name    string
	Argv    []string
	Timeout time.Duration
}

type Delegation struct {
	// Runtime is an alias in Deps.Runtimes.
	Runtime string
	Prompt  string
	Workdir string
	Model   string
	Mode    string
	Checks  []Check
	ReplyTo string
}

type runtimeSession struct {
	Runtime   string `json:"runtime"`
	SessionID string `json:"session_id"`
	Ref       string `json:"ref"`
}

type guardVerdict struct {
	RequestID string        `json:"request_id"`
	Tool      string        `json:"tool"`
	Kind      string        `json:"kind"`
	Action    policy.Action `json:"action"`
	Rule      string        `json:"rule,omitempty"`
}

type checkRun struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"`
	ExitCode   int      `json:"exit_code"`
	Err        string   `json:"err,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Output     string   `json:"output"`
}

type contractVerdict struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"`
}

type delegatedDecision struct{ approve bool }

// delegated is the handle of a live session: HandleApproval and CancelDelegated reach its loop through it.
type delegated struct {
	decide chan delegatedDecision
	cancel chan struct{}
}

type sendResult struct {
	stop StopReason
	err  error
}

// delegateFailure ends the session and fails the task with reason; any other error aborts as if the kernel died.
type delegateFailure struct{ reason string }

func (f delegateFailure) Error() string { return f.reason }

// RunDelegated runs job on a runtime and blocks until the task is done or failed.
func (a *App) RunDelegated(ctx context.Context, job Job, d Delegation) (string, error) {
	rt, ok := a.d.Runtimes[d.Runtime]
	switch {
	case !ok:
		return "", fmt.Errorf("run delegated: runtime %q is not configured", d.Runtime)
	case a.d.WorkdirState == nil:
		return "", errors.New("run delegated: no workdir probe configured")
	case len(d.Checks) > 0 && a.d.RunCheck == nil:
		return "", errors.New("run delegated: checks declared but no check runner configured")
	}
	job.Runtime = d.Runtime
	id, err := a.createTask(ctx, newID(job.Name), job, d.Prompt, d.ReplyTo, d.Workdir)
	if err != nil {
		return "", err
	}
	return id, a.runDelegated(ctx, id, rt, d)
}

// CancelDelegated stops the live session of a delegated task; the task then fails as cancelled.
func (a *App) CancelDelegated(_ context.Context, taskID string) error {
	v, ok := a.live.Load(taskID)
	if !ok {
		return fmt.Errorf("cancel task %s: no live runtime session", taskID)
	}
	select {
	case v.(*delegated).cancel <- struct{}{}:
	default:
	}
	return nil
}

func (a *App) runDelegated(ctx context.Context, id string, rt Runtime, d Delegation) error {
	if err := a.setState(ctx, id, task.Running, ""); err != nil {
		return err
	}
	before, err := a.d.WorkdirState(ctx, d.Workdir)
	if err != nil {
		return a.fail(ctx, id, truncate("probe workdir: "+err.Error(), maxFailReasonBytes))
	}
	sessionID := newID("sess")
	sess, err := rt.Start(ctx, RuntimeStart{TaskID: id, SessionID: sessionID, Workdir: d.Workdir, Model: d.Model, Mode: d.Mode})
	if err != nil {
		return a.fail(ctx, id, truncate("start runtime: "+err.Error(), maxFailReasonBytes))
	}
	stop := func() { _ = sess.Stop(context.WithoutCancel(ctx)) }
	defer stop()
	live := &delegated{decide: make(chan delegatedDecision, 1), cancel: make(chan struct{}, 1)}
	a.live.Store(id, live)
	defer a.live.Delete(id)
	if _, err := a.d.Events.Append(ctx, id, task.EvRuntimeSession, runtimeSession{Runtime: rt.Name(), SessionID: sessionID, Ref: sess.Ref()}); err != nil {
		return fmt.Errorf("append runtime session: %w", err)
	}

	r := &delegatedRun{a: a, id: id, sess: sess, live: live, sendDone: make(chan sendResult, 1)}
	go func() {
		stop, err := sess.Send(ctx, d.Prompt)
		r.sendDone <- sendResult{stop, err}
	}()
	err = r.loop(ctx)
	var f delegateFailure
	switch {
	case errors.As(err, &f):
		return a.fail(ctx, id, truncate(f.reason, maxFailReasonBytes))
	case err != nil:
		return err
	case r.cancelled:
		return a.fail(ctx, id, "cancelled")
	case r.res.err != nil:
		return a.fail(ctx, id, truncate("runtime session failed: "+r.res.err.Error(), maxFailReasonBytes))
	}
	stop()
	return a.concludeDelegated(ctx, id, d, before, r)
}

type delegatedRun struct {
	a        *App
	id       string
	sess     RuntimeSession
	live     *delegated
	sendDone chan sendResult

	res       *sendResult
	cancelled bool
	rejected  bool
	buf       RuntimeEvent
	lastText  string
}

func (r *delegatedRun) loop(ctx context.Context) error {
	events := r.sess.Events()
	for r.res == nil && !r.cancelled {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := r.onEvent(ctx, ev, false); err != nil {
				return err
			}
		case res := <-r.sendDone:
			r.res = &res
		case <-r.live.cancel:
			r.cancelled = true
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if r.cancelled && r.res == nil {
		_ = r.sess.Stop(context.WithoutCancel(ctx))
		select {
		case <-r.sendDone:
		case <-time.After(cancelGrace):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for drained := false; events != nil && !drained; {
		select {
		case ev, ok := <-events:
			if !ok {
				drained = true
				continue
			}
			if err := r.onEvent(ctx, ev, true); err != nil {
				return err
			}
		default:
			drained = true
		}
	}
	return r.flush(ctx)
}

func (r *delegatedRun) onEvent(ctx context.Context, ev RuntimeEvent, drain bool) error {
	if ev.Kind == RuntimeMessage || ev.Kind == RuntimeThought {
		if r.buf.Kind != ev.Kind || r.buf.ID != ev.ID {
			if err := r.flush(ctx); err != nil {
				return err
			}
			r.buf = RuntimeEvent{Kind: ev.Kind, ID: ev.ID}
		}
		r.buf.Text += ev.Text
		return nil
	}
	if err := r.flush(ctx); err != nil {
		return err
	}
	if err := r.record(ctx, ev); err != nil {
		return err
	}
	if ev.Kind == RuntimePermissionRequest && ev.Permission != nil && !drain {
		return r.permission(ctx, *ev.Permission)
	}
	return nil
}

func (r *delegatedRun) flush(ctx context.Context) error {
	if r.buf.Kind == "" {
		return nil
	}
	ev := r.buf
	r.buf = RuntimeEvent{}
	if ev.Kind == RuntimeMessage {
		r.lastText = ev.Text
	}
	return r.record(ctx, ev)
}

func (r *delegatedRun) record(ctx context.Context, ev RuntimeEvent) error {
	if _, err := r.a.d.Events.Append(ctx, r.id, task.EvRuntimeEvent, ev); err != nil {
		return fmt.Errorf("append runtime event: %w", err)
	}
	return r.a.hook("app.delegated_event")
}

func (r *delegatedRun) permission(ctx context.Context, p PermissionRequest) error {
	v := r.a.d.Guard.Verdict(policy.GuardRequest{Kind: p.Kind, Text: p.Text})
	if _, err := r.a.d.Events.Append(ctx, r.id, task.EvGuardVerdict, guardVerdict{RequestID: p.RequestID, Tool: p.Tool, Kind: p.Kind, Action: v.Action, Rule: v.Rule}); err != nil {
		return fmt.Errorf("append guard verdict: %w", err)
	}
	slog.Info("runtime guard", "task", r.id, "request", p.RequestID, "kind", p.Kind, "action", v.Action, "rule", v.Rule)
	switch v.Action {
	case policy.Allow:
		return r.answer(ctx, p, p.AllowOptionID)
	case policy.Deny:
		r.rejected = true
		return r.answer(ctx, p, p.RejectOptionID)
	}
	return r.ask(ctx, p, v.Rule)
}

func (r *delegatedRun) answer(ctx context.Context, p PermissionRequest, optionID string) error {
	if err := r.a.hook("app.delegated_before_answer"); err != nil {
		return err
	}
	if err := r.sess.Approve(ctx, p.RequestID, optionID); err != nil {
		return delegateFailure{"answer permission request: " + err.Error()}
	}
	return nil
}

func (r *delegatedRun) ask(ctx context.Context, p PermissionRequest, rule string) error {
	a := r.a
	raw, err := json.Marshal(struct{ Tool, Kind, Text string }{p.Tool, p.Kind, p.Text})
	if err != nil {
		return fmt.Errorf("marshal permission request: %w", err)
	}
	ap := policy.Approval{
		ID: "ap-" + p.RequestID, Tool: "runtime." + p.Kind, Target: p.Text, ArgsHash: policy.ArgsHash(raw),
		Expiry: a.d.Clock().Add(a.d.ApprovalTTL), PolicyVersion: a.d.Policy.Version,
	}
	rec := approvalRequested{CallID: p.RequestID, Approval: ap, Tier: policy.TierHighImpact, Preview: fmt.Sprintf("%s: %s (guarded: %s)", p.Kind, truncate(p.Text, 300), rule)}
	if _, err := a.d.Events.Append(ctx, r.id, task.EvApprovalRequested, rec); err != nil {
		return fmt.Errorf("append approval request: %w", err)
	}
	if err := a.setState(ctx, r.id, task.WaitingApproval, "approval "+ap.ID); err != nil {
		return err
	}
	if err := a.notifyApproval(ctx, r.id, rec); err != nil {
		return err
	}
	select {
	case dec := <-r.live.decide:
		r.rejected = r.rejected || !dec.approve
		if dec.approve {
			return r.answer(ctx, p, p.AllowOptionID)
		}
		return r.answer(ctx, p, p.RejectOptionID)
	case res := <-r.sendDone:
		r.res = &res
	case <-r.live.cancel:
		r.cancelled = true
	case <-ctx.Done():
		return ctx.Err()
	}
	return r.abandon(ctx, ap.ID)
}

// abandon returns a task whose pending approval will never be answered to running, so it can fail.
func (r *delegatedRun) abandon(ctx context.Context, approvalID string) error {
	if _, err := r.a.d.Events.Append(ctx, r.id, task.EvApprovalDecided, approvalDecided{ID: approvalID, Approved: false}); err != nil {
		return fmt.Errorf("append approval decision: %w", err)
	}
	return r.a.setState(ctx, r.id, task.Running, "session ended")
}

// answerDelegated hands a user's decision to the live session's loop; the loop answers the runtime.
func (a *App) answerDelegated(ctx context.Context, taskID string, live *delegated, req approvalRequested, approve bool) error {
	ap := req.Approval
	if err := policy.ValidateApproval(ap, ap.Tool, ap.Target, ap.ArgsHash, a.d.Clock(), a.d.Policy.Version); err != nil {
		approve = false
		if nerr := a.notify(ctx, taskID, "alert:void:"+ap.ID, Notice{Kind: NoticeAlert, Text: "An approval was voided: " + err.Error()}); nerr != nil {
			return nerr
		}
	}
	if _, err := a.d.Events.Append(ctx, taskID, task.EvApprovalDecided, approvalDecided{ID: ap.ID, Approved: approve}); err != nil {
		return fmt.Errorf("append approval decision: %w", err)
	}
	if err := a.setState(ctx, taskID, task.Running, "approval decided"); err != nil {
		return err
	}
	select {
	case live.decide <- delegatedDecision{approve: approve}:
		return nil
	default:
		return fmt.Errorf("task %s: a decision is already pending", taskID)
	}
}

func (a *App) concludeDelegated(ctx context.Context, id string, d Delegation, before string, r *delegatedRun) error {
	after, err := a.d.WorkdirState(ctx, d.Workdir)
	if err != nil {
		return a.fail(ctx, id, truncate("probe workdir: "+err.Error(), maxFailReasonBytes))
	}
	in := contract.Input{Stop: string(r.res.stop), Rejected: r.rejected, Changed: after != before}
	v := contract.Evaluate(in)
	if v.Pass && len(d.Checks) > 0 {
		if in.Checks, err = a.runChecks(ctx, id, d); err != nil {
			return err
		}
		v = contract.Evaluate(in)
	}
	if _, err := a.d.Events.Append(ctx, id, task.EvContract, contractVerdict{Pass: v.Pass, Reason: v.Reason}); err != nil {
		return fmt.Errorf("append contract verdict: %w", err)
	}
	if !v.Pass {
		return a.fail(ctx, id, truncate(v.Reason, maxFailReasonBytes))
	}
	if err := a.setState(ctx, id, task.Done, ""); err != nil {
		return err
	}
	text := r.lastText
	if text == "" {
		text = "The delegated session finished."
	}
	return a.notify(ctx, id, "final", Notice{Kind: NoticeMessage, Text: capResult(text)})
}

// runChecks runs the declared checks in order and stops at the first failure.
func (a *App) runChecks(ctx context.Context, id string, d Delegation) ([]contract.Check, error) {
	var out []contract.Check
	for _, c := range d.Checks {
		start := a.d.Clock()
		exit, output, err := a.d.RunCheck(ctx, d.Workdir, c.Argv, []string{"HAYTHOS_TASK=" + id}, c.Timeout)
		res := contract.Check{Name: c.Name, ExitCode: exit}
		rec := checkRun{Name: c.Name, Argv: c.Argv, ExitCode: exit, Output: tailCap(output, maxCheckOutput)}
		if err != nil {
			res.Err, rec.Err = truncate(err.Error(), 200), truncate(err.Error(), 200)
		}
		rec.DurationMS = a.d.Clock().Sub(start).Milliseconds()
		if _, err := a.d.Events.Append(ctx, id, task.EvCheckRun, rec); err != nil {
			return nil, fmt.Errorf("append check run: %w", err)
		}
		out = append(out, res)
		if res.Err != "" || res.ExitCode != 0 {
			break
		}
	}
	return out, nil
}

// tailCap keeps the last n bytes of s, since test failures print last.
func tailCap(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}

// failLostSessions fails delegated tasks found unfinished at startup: their session died with the old process.
func (a *App) failLostSessions(ctx context.Context) error {
	for _, st := range []task.State{task.Pending, task.Running, task.WaitingApproval} {
		ids, err := a.d.Events.TasksIn(ctx, st)
		if err != nil {
			return fmt.Errorf("list %s tasks: %w", st, err)
		}
		for _, id := range ids {
			s, err := a.Snapshot(ctx, id)
			if err != nil {
				return err
			}
			if _, live := a.live.Load(id); s.Created.Runtime == "" || live {
				continue
			}
			if err := a.failLost(ctx, id, s.State); err != nil {
				return fmt.Errorf("fail lost session %s: %w", id, err)
			}
		}
	}
	return nil
}

func (a *App) failLost(ctx context.Context, id string, st task.State) error {
	unlock := a.lock(id)
	defer unlock()
	const reason = "session lost on restart"
	if st != task.Running {
		if err := a.setState(ctx, id, task.Running, reason); err != nil {
			return err
		}
	}
	return a.fail(ctx, id, reason)
}
