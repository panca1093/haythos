package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/panca/haythos/core/task"
)

const (
	evInboxMsg       = task.EventType("inbox.msg")
	evInboxAck       = task.EventType("inbox.ack")
	evInboxTake      = task.EventType("inbox.take")
	evInboxInterrupt = task.EventType("inbox.interrupt")
	laneStreamPrefix = "inbox:"
	stoppedAnswer    = "[stopped by the owner before finishing]"
)

var (
	ErrInterrupted    = errors.New("interrupted by the owner")
	ErrNothingRunning = errors.New("nothing is running in this conversation")
	errLaneStopped    = errors.New("lane stopped before the message ran")
)

type inboxMsg struct {
	Text    string `json:"text"`
	DM      bool   `json:"dm,omitempty"`
	Workdir string `json:"workdir,omitempty"`
	Model   string `json:"model,omitempty"`
}

type inboxAck struct {
	Through int64  `json:"through"`
	Task    string `json:"task"`
}

// inboxTake fixes a turn's boundary before its task exists, so a resumed lane runs the same messages as the same task.
type inboxTake struct {
	Through int64  `json:"through"`
	Task    string `json:"task"`
}

type inboxInterrupt struct {
	Reason string `json:"reason"`
	Task   string `json:"task"`
}

// lane is one conversation's worker; its fields are guarded by App.lanesMu.
type lane struct {
	wake chan struct{}
	// acked is closed and replaced on every ack, and closed for good when the worker exits.
	acked  chan struct{}
	task   string
	cancel context.CancelCauseFunc
	done   bool
	err    error
}

func inboxStream(replyTo string) string { return laneStreamPrefix + replyTo }

// enqueue makes in durable on its lane, then blocks until an ack covers it and returns the task that ran it.
func (a *App) enqueue(ctx context.Context, in Input) (string, error) {
	ev, err := a.d.Events.Append(ctx, inboxStream(in.ReplyTo), evInboxMsg, inboxMsg{Text: in.Text, DM: in.DM, Workdir: in.Workdir, Model: in.Model})
	if err != nil {
		return "", fmt.Errorf("append inbox message: %w", err)
	}
	l := a.wakeLane(context.WithoutCancel(ctx), in.ReplyTo)
	for {
		a.lanesMu.Lock()
		ch, done, lerr := l.acked, l.done, l.err
		a.lanesMu.Unlock()
		id, ok, err := a.ackedBy(ctx, in.ReplyTo, ev.Cursor)
		switch {
		case err != nil:
			return "", err
		case ok:
			return id, nil
		case done && lerr != nil:
			return "", fmt.Errorf("run inbox %s: %w", in.ReplyTo, lerr)
		case done:
			return "", fmt.Errorf("run inbox %s: %w", in.ReplyTo, errLaneStopped)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// wakeLane starts the lane's worker on ctx if none runs, and tells it there is work.
func (a *App) wakeLane(ctx context.Context, replyTo string) *lane {
	a.lanesMu.Lock()
	defer a.lanesMu.Unlock()
	l := a.lanes[replyTo]
	if l == nil {
		l = &lane{wake: make(chan struct{}, 1), acked: make(chan struct{})}
		a.lanes[replyTo] = l
		go a.drain(ctx, replyTo, l)
	}
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return l
}

func (a *App) drain(ctx context.Context, replyTo string, l *lane) {
	for {
		select {
		case <-l.wake:
		default:
		}
		more, err := a.drainOnce(ctx, replyTo, l)
		if err != nil {
			slog.Error("inbox lane stopped", "conversation", replyTo, "err", err)
		}
		if more && err == nil {
			continue
		}
		a.lanesMu.Lock()
		if err == nil && len(l.wake) > 0 {
			a.lanesMu.Unlock()
			continue
		}
		l.done, l.err = true, err
		close(l.acked)
		delete(a.lanes, replyTo)
		a.lanesMu.Unlock()
		return
	}
}

// drainOnce runs every unacked message of the lane as one task and acks them; false means the lane was empty.
func (a *App) drainOnce(ctx context.Context, replyTo string, l *lane) (bool, error) {
	events, err := a.d.Events.Load(ctx, inboxStream(replyTo))
	if err != nil {
		return false, fmt.Errorf("load inbox %s: %w", replyTo, err)
	}
	through, err := lastAck(events)
	if err != nil {
		return false, err
	}
	take, taken, err := openTake(events)
	if err != nil {
		return false, err
	}
	var last int64
	var msgs []inboxMsg
	for _, e := range events {
		if e.Type != evInboxMsg || e.Cursor <= through || (taken && e.Cursor > take.Through) {
			continue
		}
		var m inboxMsg
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			return false, fmt.Errorf("decode inbox message %d: %w", e.Cursor, err)
		}
		msgs, last = append(msgs, m), e.Cursor
	}
	if len(msgs) == 0 {
		return false, nil
	}
	texts := make([]string, len(msgs))
	for i, m := range msgs {
		texts[i] = m.Text
	}
	m := msgs[len(msgs)-1]
	in := Input{Text: strings.Join(texts, "\n\n"), Model: m.Model, ReplyTo: replyTo, DM: m.DM, Workdir: m.Workdir}
	id := fmt.Sprintf("chat-%d", last)
	if taken {
		last, id = take.Through, take.Task
	} else if _, err := a.d.Events.Append(ctx, inboxStream(replyTo), evInboxTake, inboxTake{Through: last, Task: id}); err != nil {
		return false, fmt.Errorf("append inbox take: %w", err)
	}
	if err := a.createInboxTask(ctx, id, in); err != nil {
		return false, err
	}

	turnCtx, cancel := context.WithCancelCause(ctx)
	a.lanesMu.Lock()
	l.task, l.cancel = id, cancel
	a.lanesMu.Unlock()
	err = a.RunTask(turnCtx, id)
	a.lanesMu.Lock()
	l.task, l.cancel = "", nil
	a.lanesMu.Unlock()
	interrupted := errors.Is(context.Cause(turnCtx), ErrInterrupted)
	cancel(nil)
	if interrupted {
		err = a.stopInterrupted(ctx, id)
	}
	if err != nil {
		return false, err
	}

	if err := a.hook("inbox.before_ack"); err != nil {
		return false, err
	}
	if _, err := a.d.Events.Append(ctx, inboxStream(replyTo), evInboxAck, inboxAck{Through: last, Task: id}); err != nil {
		return false, fmt.Errorf("append inbox ack: %w", err)
	}
	a.lanesMu.Lock()
	close(l.acked)
	l.acked = make(chan struct{})
	a.lanesMu.Unlock()
	return true, nil
}

// createInboxTask is idempotent per id, so a lane resumed after a crash finds the task it had created.
func (a *App) createInboxTask(ctx context.Context, id string, in Input) error {
	existing, err := a.d.Events.Load(ctx, id)
	if err != nil {
		return fmt.Errorf("check task %s: %w", id, err)
	}
	if len(existing) > 0 {
		return nil
	}
	job := a.d.DefaultJob
	if in.Model != "" {
		job.Model = in.Model
	}
	prior, err := a.conversation(ctx, in, job.Model)
	if err != nil {
		return err
	}
	_, err = a.createTask(ctx, id, job, in.Text, in.ReplyTo, in.Workdir, prior...)
	return err
}

func (a *App) ackedBy(ctx context.Context, replyTo string, cursor int64) (string, bool, error) {
	events, err := a.d.Events.Load(ctx, inboxStream(replyTo))
	if err != nil {
		return "", false, fmt.Errorf("load inbox %s: %w", replyTo, err)
	}
	for _, e := range events {
		var ack inboxAck
		if e.Type == evInboxAck && json.Unmarshal(e.Payload, &ack) == nil && ack.Through >= cursor {
			return ack.Task, true, nil
		}
	}
	return "", false, nil
}

// stopInterrupted fails a task its lane cancelled and keeps the stopped request as conversation context.
func (a *App) stopInterrupted(ctx context.Context, id string) error {
	unlock := a.lock(id)
	defer unlock()
	s, err := a.Snapshot(ctx, id)
	if err != nil {
		return err
	}
	if s.State != task.Running {
		return nil
	}
	if err := a.recordTurn(ctx, id, stoppedAnswer, true); err != nil {
		return err
	}
	if err := a.setState(ctx, id, task.Failed, "interrupted"); err != nil {
		return err
	}
	return a.notify(ctx, id, "stopped", Notice{Kind: NoticeState, State: task.Failed, Reason: "interrupted", Text: "Stopped."})
}

// Interrupt stops the running turn of the conversation replyTo; its queued messages still run.
func (a *App) Interrupt(ctx context.Context, replyTo string) error {
	a.lanesMu.Lock()
	var id string
	var cancel context.CancelCauseFunc
	if l := a.lanes[replyTo]; l != nil {
		id, cancel = l.task, l.cancel
	}
	a.lanesMu.Unlock()
	delegated := false
	if id == "" {
		id, delegated = a.liveDelegatedFor(ctx, replyTo), true
	}
	if id == "" {
		return ErrNothingRunning
	}
	if _, err := a.d.Events.Append(ctx, inboxStream(replyTo), evInboxInterrupt, inboxInterrupt{Reason: "stopped by the owner", Task: id}); err != nil {
		return fmt.Errorf("append inbox interrupt: %w", err)
	}
	if delegated {
		return a.CancelDelegated(ctx, id)
	}
	cancel(ErrInterrupted)
	return nil
}

func (a *App) liveDelegatedFor(ctx context.Context, replyTo string) string {
	var found string
	a.live.Range(func(k, _ any) bool {
		if s, err := a.Snapshot(ctx, k.(string)); err == nil && s.Created.ReplyTo == replyTo {
			found = k.(string)
			return false
		}
		return true
	})
	return found
}

// resumeLanes starts a worker on ctx for every inbox that has a message after its last ack.
func (a *App) resumeLanes(ctx context.Context) error {
	events, err := a.d.Events.Since(ctx, 0)
	if err != nil {
		return fmt.Errorf("scan inboxes: %w", err)
	}
	byLane := map[string][]task.Event{}
	for _, e := range events {
		if replyTo, ok := strings.CutPrefix(e.TaskID, laneStreamPrefix); ok {
			byLane[replyTo] = append(byLane[replyTo], e)
		}
	}
	for replyTo, evs := range byLane {
		through, err := lastAck(evs)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if e.Type == evInboxMsg && e.Cursor > through {
				a.wakeLane(ctx, replyTo)
				break
			}
		}
	}
	return nil
}

// openTake is the lane's last take when no ack follows it: a turn that started and was not acked.
func openTake(events []task.Event) (inboxTake, bool, error) {
	var take inboxTake
	open := false
	for _, e := range events {
		switch e.Type {
		case evInboxTake:
			if err := json.Unmarshal(e.Payload, &take); err != nil {
				return inboxTake{}, false, fmt.Errorf("decode inbox take %d: %w", e.Cursor, err)
			}
			open = true
		case evInboxAck:
			open = false
		}
	}
	return take, open, nil
}

// lastAck is the lane's offset: the cursor its last ack covers.
func lastAck(events []task.Event) (int64, error) {
	var through int64
	for _, e := range events {
		if e.Type != evInboxAck {
			continue
		}
		var ack inboxAck
		if err := json.Unmarshal(e.Payload, &ack); err != nil {
			return 0, fmt.Errorf("decode inbox ack %d: %w", e.Cursor, err)
		}
		through = ack.Through
	}
	return through, nil
}
