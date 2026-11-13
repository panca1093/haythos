package testfakes

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/panca/haythos/app"
)

var ErrRuntimeCrashed = errors.New("fake runtime: session crashed")

// RuntimeStep is one scripted action of a turn; set exactly one field.
type RuntimeStep struct {
	emit  *app.RuntimeEvent
	do    func(workdir string, allowed bool)
	crash bool
	hang  bool
}

// Emit sends an event; a permission_request blocks the turn until Approve answers it.
func Emit(ev app.RuntimeEvent) RuntimeStep { return RuntimeStep{emit: &ev} }

// Do runs a side effect in the workdir; allowed is false once any request of the turn was rejected.
func Do(f func(workdir string, allowed bool)) RuntimeStep { return RuntimeStep{do: f} }

// Crash closes Events and fails Send with ErrRuntimeCrashed.
func Crash() RuntimeStep { return RuntimeStep{crash: true} }

// Hang blocks the turn until Stop.
func Hang() RuntimeStep { return RuntimeStep{hang: true} }

// RuntimeScript is one Send: its steps, then a stop event with Stop (default end_turn).
type RuntimeScript struct {
	Steps []RuntimeStep
	Stop  app.StopReason
	// CancelOnReject ends the turn as cancelled after a rejected request, as opencode does.
	CancelOnReject bool
}

type Runtime struct {
	name string
	mu   sync.Mutex
	// scripts are consumed one per Send, across all sessions.
	scripts  []RuntimeScript
	sessions []*RuntimeSession
	StartErr error
	Caps     app.RuntimeCaps
}

func NewRuntime(name string, scripts ...RuntimeScript) *Runtime {
	return &Runtime{name: name, scripts: scripts}
}

func (r *Runtime) Name() string                  { return r.name }
func (r *Runtime) Capabilities() app.RuntimeCaps { return r.Caps }

func (r *Runtime) Start(ctx context.Context, req app.RuntimeStart) (app.RuntimeSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.StartErr != nil {
		return nil, r.StartErr
	}
	s := &RuntimeSession{
		rt: r, req: req, events: make(chan app.RuntimeEvent, 256), stopCh: make(chan struct{}),
		pending: map[string]chan string{}, ref: fmt.Sprintf("ref-%d", len(r.sessions)+1),
	}
	r.sessions = append(r.sessions, s)
	return s, nil
}

// Session is the i-th session started, for assertions.
func (r *Runtime) Session(i int) *RuntimeSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[i]
}

func (r *Runtime) Sessions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *Runtime) nextScript() (RuntimeScript, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.scripts) == 0 {
		return RuntimeScript{}, false
	}
	s := r.scripts[0]
	r.scripts = r.scripts[1:]
	return s, true
}

type ApproveCall struct{ RequestID, OptionID string }

type RuntimeSession struct {
	rt  *Runtime
	req app.RuntimeStart
	ref string

	events   chan app.RuntimeEvent
	stopCh   chan struct{}
	stopOnce sync.Once

	emitMu sync.RWMutex
	closed bool

	mu        sync.Mutex
	pending   map[string]chan string
	approvals []ApproveCall
	prompts   []string
	stopped   bool
}

func (s *RuntimeSession) Ref() string                     { return s.ref }
func (s *RuntimeSession) Events() <-chan app.RuntimeEvent { return s.events }

func (s *RuntimeSession) Start() app.RuntimeStart { return s.req }

func (s *RuntimeSession) Approvals() []ApproveCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ApproveCall(nil), s.approvals...)
}

func (s *RuntimeSession) Prompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.prompts...)
}

func (s *RuntimeSession) Stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *RuntimeSession) Approve(ctx context.Context, requestID, optionID string) error {
	s.mu.Lock()
	ch, ok := s.pending[requestID]
	if ok {
		delete(s.pending, requestID)
		s.approvals = append(s.approvals, ApproveCall{requestID, optionID})
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("approve %s: no such pending request", requestID)
	}
	ch <- optionID
	return nil
}

func (s *RuntimeSession) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.closeEvents()
	return nil
}

func (s *RuntimeSession) closeEvents() {
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
	}
}

func (s *RuntimeSession) emit(ctx context.Context, ev app.RuntimeEvent) {
	s.emitMu.RLock()
	defer s.emitMu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	case <-s.stopCh:
	case <-ctx.Done():
	}
}

func (s *RuntimeSession) Send(ctx context.Context, prompt string) (app.StopReason, error) {
	script, ok := s.rt.nextScript()
	if !ok {
		return "", errors.New("fake runtime: script exhausted")
	}
	s.mu.Lock()
	s.prompts = append(s.prompts, prompt)
	s.mu.Unlock()
	allowed := true
	for _, step := range script.Steps {
		switch {
		case step.emit != nil:
			var answer chan string
			if p := step.emit.Permission; step.emit.Kind == app.RuntimePermissionRequest && p != nil {
				answer = make(chan string, 1)
				s.mu.Lock()
				s.pending[p.RequestID] = answer
				s.mu.Unlock()
			}
			s.emit(ctx, *step.emit)
			if answer == nil {
				continue
			}
			select {
			case opt := <-answer:
				if opt != step.emit.Permission.AllowOptionID {
					allowed = false
				}
			case <-s.stopCh:
				return app.StopCancelled, nil
			case <-ctx.Done():
				return "", fmt.Errorf("await approval: %w", ctx.Err())
			}
			if !allowed && script.CancelOnReject {
				s.emit(ctx, app.RuntimeEvent{Kind: app.RuntimeStop, Stop: app.StopCancelled})
				return app.StopCancelled, nil
			}
		case step.do != nil:
			step.do(s.req.Workdir, allowed)
		case step.crash:
			s.closeEvents()
			return "", ErrRuntimeCrashed
		case step.hang:
			select {
			case <-s.stopCh:
				return app.StopCancelled, nil
			case <-ctx.Done():
				return "", fmt.Errorf("hang: %w", ctx.Err())
			}
		}
	}
	stop := script.Stop
	if stop == "" {
		stop = app.StopEndTurn
	}
	s.emit(ctx, app.RuntimeEvent{Kind: app.RuntimeStop, Stop: stop})
	return stop, nil
}
