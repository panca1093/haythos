// Package codexapp drives `codex app-server` (JSON-RPC over stdio, native, no Node) as an app.Runtime.
//
// Every session runs in a fresh CODEX_HOME that holds only a link to the owner's auth.json, so the owner's hooks,
// MCP servers, AGENTS.md and project trust do not leak into a delegated task. Threads are ephemeral, so resume is
// not supported.
package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/panca/haythos/adapters/runtime/internal/jsonrpc"
	"github.com/panca/haythos/app"
)

const (
	defaultGrace = 2 * time.Second
	stderrKeep   = 2048

	// untrusted asks for every command except a short list of known-safe reads; the approval is the guard, so the
	// sandbox stays open (rule 2: no sandbox until the owner asks).
	approvalPolicy = "untrusted"
	sandboxMode    = "danger-full-access"

	optionAccept  = "accept"
	optionDecline = "decline"
	optionCancel  = "cancel"
)

// Config is the codex runtime entry.
type Config struct {
	// Name is what Runtime.Name returns; default "codex".
	Name string
	// Command and Args start the app-server; defaults "codex" and ["app-server"].
	Command string
	Args    []string
	// Model is used when RuntimeStart names none; empty lets codex choose.
	Model string
	// PassEnv names variables copied from the kernel's environment into the child, on top of PATH, HOME and USER.
	PassEnv []string
	// Env is extra child environment as KEY=VALUE.
	Env []string
	// CodexHome is the owner's codex directory; only its auth.json is linked into each session's own CODEX_HOME.
	// Default $HOME/.codex.
	CodexHome string
	// StopGrace is how long Stop waits for an interrupted turn to end before killing the process.
	StopGrace time.Duration
}

type Runtime struct {
	cfg Config
}

var _ app.Runtime = (*Runtime)(nil)

func New(cfg Config) *Runtime {
	if cfg.Name == "" {
		cfg.Name = "codex"
	}
	if cfg.Command == "" {
		cfg.Command = "codex"
	}
	if cfg.Args == nil {
		cfg.Args = []string{"app-server"}
	}
	if cfg.CodexHome == "" {
		cfg.CodexHome = filepath.Join(os.Getenv("HOME"), ".codex")
	}
	if cfg.StopGrace == 0 {
		cfg.StopGrace = defaultGrace
	}
	return &Runtime{cfg: cfg}
}

func (r *Runtime) Name() string { return r.cfg.Name }

// Capabilities: no resume (ephemeral threads in a throwaway CODEX_HOME), no images, no modes.
func (r *Runtime) Capabilities() app.RuntimeCaps { return app.RuntimeCaps{} }

func (r *Runtime) childEnv(req app.RuntimeStart, home string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER"),
		"HAYTHOS_TASK=" + req.TaskID, "HAYTHOS_SESSION=" + req.SessionID, "CODEX_HOME=" + home,
	}
	for _, name := range r.cfg.PassEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return append(env, r.cfg.Env...)
}

// newHome makes the session's CODEX_HOME with a symlink to the owner's auth.json. A missing auth.json is not an
// error here: codex then fails on its own with a login message that Start reports.
func (r *Runtime) newHome() (string, error) {
	home, err := os.MkdirTemp("", "haythos-codex-*")
	if err != nil {
		return "", fmt.Errorf("make codex home: %w", err)
	}
	src := filepath.Join(r.cfg.CodexHome, "auth.json")
	if _, err := os.Stat(src); err == nil {
		if err := os.Symlink(src, filepath.Join(home, "auth.json")); err != nil {
			_ = os.RemoveAll(home)
			return "", fmt.Errorf("link auth.json: %w", err)
		}
	}
	return home, nil
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
	if req.ResumeRef != "" {
		return nil, errors.New("codex sessions cannot be resumed (threads are ephemeral)")
	}
	home, err := r.newHome()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(context.WithoutCancel(ctx), r.cfg.Command, r.cfg.Args...) //nolint:gosec // command comes from the owner's config; Stop owns the lifetime
	cmd.Dir = req.Workdir
	cmd.Env = r.childEnv(req, home)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	errTail := &tail{}
	cmd.Stderr = errTail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("pipe %s stdin: %w", r.cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("pipe %s stdout: %w", r.cfg.Name, err)
	}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(home)
		return nil, fmt.Errorf("start %s: %w", r.cfg.Name, err)
	}
	s := &session{
		rt: r, cmd: cmd, stdin: stdin, home: home, events: make(chan app.RuntimeEvent, 256), dead: make(chan struct{}),
		pending: map[string]json.RawMessage{}, fileItems: map[string]string{},
	}
	s.conn = jsonrpc.NewConn(stdout, stdin, s, jsonrpc.Options{OmitVersion: true})
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

type turnResult struct {
	stop app.StopReason
	err  error
}

type session struct {
	rt    *Runtime
	cmd   *exec.Cmd
	stdin interface{ Close() error }
	conn  *jsonrpc.Conn
	home  string

	tid string

	events    chan app.RuntimeEvent
	dead      chan struct{}
	deadOnce  sync.Once
	emitMu    sync.RWMutex
	evClosed  bool
	stopOnce  sync.Once
	killOnce  sync.Once
	stopped   atomic.Bool
	pendingMu sync.Mutex
	pending   map[string]json.RawMessage

	// fileItems maps a fileChange item id to its paths; touched only on the read loop.
	fileItems map[string]string

	turnMu    sync.Mutex
	turnID    string
	turnEnded chan struct{}
	completed chan turnResult
}

func (s *session) Ref() string                     { return s.tid }
func (s *session) Events() <-chan app.RuntimeEvent { return s.events }

func (s *session) handshake(ctx context.Context, req app.RuntimeStart) error {
	err := s.conn.Call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "haythos", "version": "0"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, nil)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := s.conn.Notify("initialized", nil); err != nil {
		return fmt.Errorf("initialized: %w", err)
	}
	params := map[string]any{"cwd": req.Workdir, "approvalPolicy": approvalPolicy, "sandbox": sandboxMode, "ephemeral": true}
	model := req.Model
	if model == "" {
		model = s.rt.cfg.Model
	}
	if model != "" {
		params["model"] = model
	}
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := s.conn.Call(ctx, "thread/start", params, &res); err != nil {
		return fmt.Errorf("thread/start: %w", err)
	}
	if res.Thread.ID == "" {
		return errors.New("thread/start returned no thread id")
	}
	s.tid = res.Thread.ID
	return nil
}

type item struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Server  string `json:"server"`
	Tool    string `json:"tool"`
	Query   string `json:"query"`
	Changes []struct {
		Path string `json:"path"`
	} `json:"changes"`
}

func (it item) paths() string {
	ps := make([]string, 0, len(it.Changes))
	for _, c := range it.Changes {
		ps = append(ps, c.Path)
	}
	return strings.Join(ps, ", ")
}

// toolEvent describes an item as a tool call; ok is false for items that are not tool calls (messages, reasoning).
func (it item) toolEvent(kind app.RuntimeEventKind) (app.RuntimeEvent, bool) {
	ev := app.RuntimeEvent{Kind: kind, ID: it.ID, Status: toolStatus(it.Status)}
	switch it.Type {
	case "commandExecution":
		ev.ToolKind, ev.Text = "execute", it.Command
	case "fileChange":
		ev.ToolKind, ev.Text = "edit", it.paths()
	case "mcpToolCall", "dynamicToolCall":
		ev.ToolKind, ev.Text = "other", strings.TrimPrefix(it.Server+"."+it.Tool, ".")
	case "webSearch":
		ev.ToolKind, ev.Text = "fetch", it.Query
	default:
		return ev, false
	}
	return ev, true
}

func toolStatus(s string) string {
	switch s {
	case "inProgress":
		return "in_progress"
	case "completed":
		return "completed"
	case "failed", "declined":
		return "failed"
	}
	return s
}

type turnError struct {
	Message string `json:"message"`
}

// Notification maps app-server notifications to RuntimeEvents; it runs on the read loop, so events keep codex's order.
func (s *session) Notification(method string, params json.RawMessage) {
	var p struct {
		ThreadID string `json:"threadId"`
		ItemID   string `json:"itemId"`
		Delta    string `json:"delta"`
		Item     item   `json:"item"`
		Turn     struct {
			ID     string     `json:"id"`
			Status string     `json:"status"`
			Error  *turnError `json:"error"`
		} `json:"turn"`
		Plan []struct {
			Step string `json:"step"`
		} `json:"plan"`
	}
	if json.Unmarshal(params, &p) != nil || (p.ThreadID != "" && p.ThreadID != s.tid) {
		return
	}
	switch method {
	case "turn/started":
		s.turnMu.Lock()
		s.turnID = p.Turn.ID
		s.turnMu.Unlock()
	case "turn/completed":
		s.finishTurn(p.Turn.Status, p.Turn.Error)
	case "item/started":
		if p.Item.Type == "fileChange" {
			s.fileItems[p.Item.ID] = p.Item.paths()
		}
		if ev, ok := p.Item.toolEvent(app.RuntimeToolCall); ok {
			s.emit(ev)
		}
	case "item/completed":
		if ev, ok := p.Item.toolEvent(app.RuntimeToolCallUpdate); ok {
			s.emit(ev)
		}
	case "item/agentMessage/delta":
		s.emitText(app.RuntimeMessage, p.ItemID, p.Delta)
	case "item/reasoning/textDelta", "item/reasoning/summaryTextDelta":
		s.emitText(app.RuntimeThought, p.ItemID, p.Delta)
	case "turn/plan/updated":
		lines := make([]string, 0, len(p.Plan))
		for _, st := range p.Plan {
			lines = append(lines, st.Step)
		}
		s.emit(app.RuntimeEvent{Kind: app.RuntimePlan, Text: strings.Join(lines, "\n")})
	}
}

func (s *session) finishTurn(status string, turnErr *turnError) {
	res := turnResult{}
	switch status {
	case "completed":
		res.stop = app.StopEndTurn
	case "interrupted":
		res.stop = app.StopCancelled
	default:
		msg := "turn " + status
		if turnErr != nil && turnErr.Message != "" {
			msg += ": " + turnErr.Message
		}
		res.err = errors.New(msg)
	}
	s.turnMu.Lock()
	ch := s.completed
	s.turnMu.Unlock()
	if ch != nil {
		select {
		case ch <- res:
		default:
		}
	}
}

func (s *session) emitText(kind app.RuntimeEventKind, id, text string) {
	if text == "" {
		return
	}
	s.emit(app.RuntimeEvent{Kind: kind, ID: id, Text: text})
}

// Request turns approval requests into permission events and refuses everything else.
func (s *session) Request(id json.RawMessage, method string, params json.RawMessage) {
	var p struct {
		ItemID  string `json:"itemId"`
		Command string `json:"command"`
		Reason  string `json:"reason"`
	}
	if method != "item/commandExecution/requestApproval" && method != "item/fileChange/requestApproval" {
		_ = s.conn.RespondError(id, -32601, "method not found: "+method)
		return
	}
	if err := json.Unmarshal(params, &p); err != nil {
		_ = s.conn.RespondError(id, -32602, "invalid params")
		return
	}
	req := &app.PermissionRequest{
		RequestID: string(id), Tool: "commandExecution", Kind: "execute", Text: p.Command,
		AllowOptionID: optionAccept, RejectOptionID: optionDecline,
	}
	if method == "item/fileChange/requestApproval" {
		req.Tool, req.Kind, req.Text = "fileChange", "edit", s.fileItems[p.ItemID]
		if req.Text == "" {
			req.Text = p.Reason
		}
	}
	s.pendingMu.Lock()
	s.pending[req.RequestID] = id
	s.pendingMu.Unlock()
	s.emit(app.RuntimeEvent{Kind: app.RuntimePermissionRequest, ID: p.ItemID, Text: req.Text, ToolKind: req.Kind, Permission: req})
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

// Send runs one turn and returns when turn/completed arrived. One turn at a time per session.
func (s *session) Send(ctx context.Context, prompt string) (app.StopReason, error) {
	ended := make(chan struct{})
	completed := make(chan turnResult, 1)
	s.turnMu.Lock()
	s.turnEnded, s.completed, s.turnID = ended, completed, ""
	s.turnMu.Unlock()
	defer close(ended)

	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	err := s.conn.Call(ctx, "turn/start", map[string]any{
		"threadId": s.tid, "input": []any{map[string]any{"type": "text", "text": prompt}},
	}, &started)
	if err != nil {
		return "", fmt.Errorf("turn/start: %w", err)
	}
	s.turnMu.Lock()
	if started.Turn.ID != "" {
		s.turnID = started.Turn.ID
	}
	s.turnMu.Unlock()

	var res turnResult
	select {
	case res = <-completed:
	case <-s.conn.Done():
		select {
		case res = <-completed:
		default:
			if !s.stopped.Load() {
				return "", errors.New("turn: app-server exited")
			}
			res = turnResult{stop: app.StopCancelled}
		}
	case <-ctx.Done():
		s.interrupt(context.WithoutCancel(ctx))
		return "", fmt.Errorf("turn: %w", ctx.Err())
	}
	if res.err != nil {
		return "", fmt.Errorf("turn: %w", res.err)
	}
	s.emit(app.RuntimeEvent{Kind: app.RuntimeStop, Stop: res.stop})
	return res.stop, nil
}

func (s *session) interrupt(ctx context.Context) {
	s.turnMu.Lock()
	id := s.turnID
	s.turnMu.Unlock()
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.rt.cfg.StopGrace)
	defer cancel()
	_ = s.conn.Call(ctx, "turn/interrupt", map[string]any{"threadId": s.tid, "turnId": id}, nil)
}

// Approve answers a pending approval; the option ids are accept and decline, and an empty one cancels the request.
func (s *session) Approve(_ context.Context, requestID, optionID string) error {
	switch optionID {
	case "":
		optionID = optionCancel
	case optionAccept, optionDecline, optionCancel:
	default:
		return fmt.Errorf("approve %s: unknown option %q", requestID, optionID)
	}
	s.pendingMu.Lock()
	id, ok := s.pending[requestID]
	delete(s.pending, requestID)
	s.pendingMu.Unlock()
	if !ok {
		return fmt.Errorf("approve %s: no such pending request", requestID)
	}
	return s.respond(id, optionID)
}

func (s *session) respond(id json.RawMessage, decision string) error {
	if err := s.conn.Respond(id, map[string]any{"decision": decision}); err != nil {
		return fmt.Errorf("answer approval request: %w", err)
	}
	return nil
}

// Stop cancels open approvals, interrupts the running turn, gives it StopGrace to end, then kills the process group.
// Safe to call again.
func (s *session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.stopped.Store(true)
		s.pendingMu.Lock()
		pending := s.pending
		s.pending = map[string]json.RawMessage{}
		s.pendingMu.Unlock()
		for _, id := range pending {
			_ = s.respond(id, optionCancel)
		}
		s.turnMu.Lock()
		ended := s.turnEnded
		s.turnMu.Unlock()
		if ended != nil {
			select {
			case <-ended:
			default:
				s.interrupt(ctx)
				select {
				case <-ended:
				case <-s.conn.Done():
				case <-time.After(s.rt.cfg.StopGrace):
				case <-ctx.Done():
				}
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
		_ = os.RemoveAll(s.home)
	})
}
