// Package acp drives a runtime that speaks the Agent Client Protocol over stdio (opencode, goose, ...) as an app.Runtime.
// No fs/* or terminal/* client methods are offered, so the runtime uses its own tools.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/panca/haythos/adapters/runtime/internal/jsonrpc"
	"github.com/panca/haythos/app"
)

const (
	protocolVersion = 1
	defaultGrace    = 2 * time.Second
	stderrKeep      = 2048
)

// Config is one runtime entry: a command plus how its ACP session is set up.
type Config struct {
	Name    string
	Command string
	Args    []string
	// PassEnv names variables copied from the kernel's environment into the child, on top of PATH, HOME and USER.
	PassEnv []string
	// Env is extra child environment as KEY=VALUE, e.g. the runtime's own permission config.
	Env []string
	// ConfigOptions maps "model" and "mode" to the config option id set through session/set_config_option; missing keys default to the same word.
	ConfigOptions map[string]string
	// StopGrace is how long Stop waits for a running turn to end after session/cancel before killing the process.
	StopGrace time.Duration
}

type Runtime struct {
	cfg Config

	mu   sync.Mutex
	caps app.RuntimeCaps
}

var _ app.Runtime = (*Runtime)(nil)

func New(cfg Config) *Runtime {
	if cfg.StopGrace == 0 {
		cfg.StopGrace = defaultGrace
	}
	return &Runtime{cfg: cfg}
}

func (r *Runtime) Name() string { return r.cfg.Name }

// Capabilities are those the runtime reported on its latest Start; zero before the first one.
func (r *Runtime) Capabilities() app.RuntimeCaps {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caps
}

func (r *Runtime) optionID(key string) string {
	if id := r.cfg.ConfigOptions[key]; id != "" {
		return id
	}
	return key
}

func (r *Runtime) childEnv(req app.RuntimeStart) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER"),
		"HAYTHOS_TASK=" + req.TaskID, "HAYTHOS_SESSION=" + req.SessionID,
	}
	for _, name := range r.cfg.PassEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env, r.cfg.Env...)
}

type tail struct {
	mu sync.Mutex
	b  []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > stderrKeep {
		t.b = t.b[len(t.b)-stderrKeep:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.b))
}

func (r *Runtime) Start(ctx context.Context, req app.RuntimeStart) (app.RuntimeSession, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), r.cfg.Command, r.cfg.Args...) //nolint:gosec // command comes from the owner's config; Stop owns the lifetime
	cmd.Dir = req.Workdir
	cmd.Env = r.childEnv(req)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	errTail := &tail{}
	cmd.Stderr = errTail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe %s stdin: %w", r.cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("pipe %s stdout: %w", r.cfg.Name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", r.cfg.Name, err)
	}
	s := &session{
		rt: r, cmd: cmd, stdin: stdin, events: make(chan app.RuntimeEvent, 256), dead: make(chan struct{}),
		pending: map[string]json.RawMessage{},
	}
	s.conn = jsonrpc.NewConn(stdout, stdin, s, jsonrpc.Options{})
	go func() {
		<-s.conn.Done()
		_ = cmd.Wait()
		s.closeEvents()
	}()
	if err := s.handshake(ctx, req); err != nil {
		s.kill()
		if msg := errTail.String(); msg != "" {
			err = fmt.Errorf("%w (stderr: %s)", err, msg)
		}
		return nil, fmt.Errorf("start %s session: %w", r.cfg.Name, err)
	}
	slog.Info("runtime session started", "runtime", r.cfg.Name, "task", req.TaskID, "session", req.SessionID)
	return s, nil
}

type session struct {
	rt    *Runtime
	cmd   *exec.Cmd
	stdin interface{ Close() error }
	conn  *jsonrpc.Conn

	sid       string
	replaying atomic.Bool

	events    chan app.RuntimeEvent
	dead      chan struct{}
	deadOnce  sync.Once
	emitMu    sync.RWMutex
	evClosed  bool
	stopOnce  sync.Once
	killOnce  sync.Once
	pendingMu sync.Mutex
	pending   map[string]json.RawMessage

	turnMu sync.Mutex
	turn   chan struct{}
}

func (s *session) Ref() string                     { return s.sid }
func (s *session) Events() <-chan app.RuntimeEvent { return s.events }

func (s *session) handshake(ctx context.Context, req app.RuntimeStart) error {
	var init struct {
		AgentCapabilities struct {
			LoadSession        bool `json:"loadSession"`
			PromptCapabilities struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	err := s.conn.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    protocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]any{"name": "haythos", "version": "0"},
	}, &init)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	caps := app.RuntimeCaps{Resume: init.AgentCapabilities.LoadSession, Images: init.AgentCapabilities.PromptCapabilities.Image}

	var created struct {
		SessionID     string         `json:"sessionId"`
		ConfigOptions []configOption `json:"configOptions"`
	}
	if req.ResumeRef != "" {
		if !caps.Resume {
			return errors.New("runtime cannot resume sessions (no loadSession capability)")
		}
		s.sid = req.ResumeRef
		s.replaying.Store(true)
		err = s.conn.Call(ctx, "session/load", map[string]any{"sessionId": s.sid, "cwd": req.Workdir, "mcpServers": []any{}}, &created)
		s.replaying.Store(false)
		if err != nil {
			return fmt.Errorf("session/load: %w", err)
		}
	} else {
		if err := s.conn.Call(ctx, "session/new", map[string]any{"cwd": req.Workdir, "mcpServers": []any{}}, &created); err != nil {
			return fmt.Errorf("session/new: %w", err)
		}
		if created.SessionID == "" {
			return errors.New("session/new returned no session id")
		}
		s.sid = created.SessionID
	}
	for _, o := range created.ConfigOptions {
		if o.ID == s.rt.optionID("mode") {
			for _, v := range o.Options {
				caps.Modes = append(caps.Modes, v.Value)
			}
		}
	}
	for key, value := range map[string]string{"model": req.Model, "mode": req.Mode} {
		if value == "" {
			continue
		}
		err := s.conn.Call(ctx, "session/set_config_option", map[string]any{"sessionId": s.sid, "configId": s.rt.optionID(key), "value": value}, nil)
		if err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	s.rt.mu.Lock()
	s.rt.caps = caps
	s.rt.mu.Unlock()
	return nil
}

type configOption struct {
	ID      string `json:"id"`
	Options []struct {
		Value string `json:"value"`
	} `json:"options"`
}

type update struct {
	SessionUpdate string `json:"sessionUpdate"`
	MessageID     string `json:"messageId"`
	Content       struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	ToolCallID    string `json:"toolCallId"`
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	CurrentModeID string `json:"currentModeId"`
	Entries       []struct {
		Content string `json:"content"`
	} `json:"entries"`
}

// Notification maps session/update to a RuntimeEvent; it runs on the read loop, so events keep the agent's order.
func (s *session) Notification(method string, params json.RawMessage) {
	if method != "session/update" || s.replaying.Load() {
		return
	}
	var p struct {
		Update update `json:"update"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	u := p.Update
	switch u.SessionUpdate {
	case "agent_message_chunk":
		s.emitText(app.RuntimeMessage, u)
	case "agent_thought_chunk":
		s.emitText(app.RuntimeThought, u)
	case "tool_call":
		s.emit(app.RuntimeEvent{Kind: app.RuntimeToolCall, ID: u.ToolCallID, Text: u.Title, ToolKind: u.Kind, Status: u.Status})
	case "tool_call_update":
		s.emit(app.RuntimeEvent{Kind: app.RuntimeToolCallUpdate, ID: u.ToolCallID, Text: u.Title, ToolKind: u.Kind, Status: u.Status})
	case "plan":
		lines := make([]string, 0, len(u.Entries))
		for _, e := range u.Entries {
			lines = append(lines, e.Content)
		}
		s.emit(app.RuntimeEvent{Kind: app.RuntimePlan, Text: strings.Join(lines, "\n")})
	case "current_mode_update":
		s.emit(app.RuntimeEvent{Kind: app.RuntimeMode, Text: u.CurrentModeID})
	}
}

func (s *session) emitText(kind app.RuntimeEventKind, u update) {
	if u.Content.Type != "text" || u.Content.Text == "" {
		return
	}
	s.emit(app.RuntimeEvent{Kind: kind, ID: u.MessageID, Text: u.Content.Text})
}

// Request answers client methods we do not offer with method-not-found and turns permission requests into events.
func (s *session) Request(id json.RawMessage, method string, params json.RawMessage) {
	if method != "session/request_permission" {
		_ = s.conn.RespondError(id, -32601, "method not found: "+method)
		return
	}
	var p struct {
		ToolCall struct {
			ToolCallID string          `json:"toolCallId"`
			Name       string          `json:"name"`
			Title      string          `json:"title"`
			Kind       string          `json:"kind"`
			RawInput   json.RawMessage `json:"rawInput"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		_ = s.conn.RespondError(id, -32602, "invalid params")
		return
	}
	req := &app.PermissionRequest{RequestID: string(id), Tool: p.ToolCall.Name, Kind: p.ToolCall.Kind, Text: permissionText(p.ToolCall.RawInput, p.ToolCall.Title)}
	if req.Tool == "" {
		req.Tool = p.ToolCall.Title
	}
	for _, o := range p.Options {
		switch o.Kind {
		case "allow_once":
			req.AllowOptionID = o.OptionID
		case "reject_once":
			req.RejectOptionID = o.OptionID
		}
	}
	s.pendingMu.Lock()
	s.pending[req.RequestID] = id
	s.pendingMu.Unlock()
	s.emit(app.RuntimeEvent{Kind: app.RuntimePermissionRequest, ID: p.ToolCall.ToolCallID, Text: req.Text, ToolKind: req.Kind, Permission: req})
}

func permissionText(rawInput json.RawMessage, title string) string {
	var in map[string]any
	if json.Unmarshal(rawInput, &in) == nil {
		for _, k := range []string{"command", "path", "filePath", "file_path"} {
			if v, ok := in[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return title
}

func (s *session) emit(ev app.RuntimeEvent) {
	s.emitMu.RLock()
	defer s.emitMu.RUnlock()
	if s.evClosed {
		return
	}
	select {
	case s.events <- ev:
	case <-s.dead:
	}
}

func (s *session) closeEvents() {
	s.deadOnce.Do(func() { close(s.dead) })
	s.emitMu.Lock()
	defer s.emitMu.Unlock()
	if !s.evClosed {
		s.evClosed = true
		close(s.events)
	}
}

// Send runs one prompt turn. One turn at a time per session.
func (s *session) Send(ctx context.Context, prompt string) (app.StopReason, error) {
	done := make(chan struct{})
	s.turnMu.Lock()
	s.turn = done
	s.turnMu.Unlock()
	defer close(done)
	var res struct {
		StopReason string `json:"stopReason"`
	}
	err := s.conn.Call(ctx, "session/prompt", map[string]any{
		"sessionId": s.sid, "prompt": []any{map[string]any{"type": "text", "text": prompt}},
	}, &res)
	if err != nil {
		if ctx.Err() != nil {
			_ = s.conn.Notify("session/cancel", map[string]any{"sessionId": s.sid})
		}
		return "", fmt.Errorf("session/prompt: %w", err)
	}
	stop := app.StopReason(res.StopReason)
	s.emit(app.RuntimeEvent{Kind: app.RuntimeStop, Stop: stop})
	return stop, nil
}

// Approve answers a pending permission request; an empty optionID answers it as cancelled.
func (s *session) Approve(_ context.Context, requestID, optionID string) error {
	s.pendingMu.Lock()
	id, ok := s.pending[requestID]
	delete(s.pending, requestID)
	s.pendingMu.Unlock()
	if !ok {
		return fmt.Errorf("approve %s: no such pending request", requestID)
	}
	return s.respondOutcome(id, optionID)
}

func (s *session) respondOutcome(id json.RawMessage, optionID string) error {
	outcome := map[string]any{"outcome": "cancelled"}
	if optionID != "" {
		outcome = map[string]any{"outcome": "selected", "optionId": optionID}
	}
	if err := s.conn.Respond(id, map[string]any{"outcome": outcome}); err != nil {
		return fmt.Errorf("answer permission request: %w", err)
	}
	return nil
}

// Stop cancels the running turn, gives it StopGrace to end, then kills the process group. Safe to call again.
func (s *session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.pendingMu.Lock()
		pending := s.pending
		s.pending = map[string]json.RawMessage{}
		s.pendingMu.Unlock()
		for _, id := range pending {
			_ = s.respondOutcome(id, "")
		}
		_ = s.conn.Notify("session/cancel", map[string]any{"sessionId": s.sid})
		s.turnMu.Lock()
		turn := s.turn
		s.turnMu.Unlock()
		if turn != nil {
			select {
			case <-turn:
			case <-s.conn.Done():
			case <-time.After(s.rt.cfg.StopGrace):
			case <-ctx.Done():
			}
		}
		s.kill()
	})
	return nil
}

func (s *session) kill() {
	s.killOnce.Do(func() {
		_ = s.stdin.Close()
		if s.cmd.Process != nil {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-s.conn.Done():
		case <-time.After(s.rt.cfg.StopGrace):
		}
		s.closeEvents()
	})
}
