// Package claudecli runs Claude Code as a long-lived `claude -p` process speaking stream-json both ways and implements app.Runtime.
// Only the official CLI is spawned (CLAUDE.md rule 5). Every shell command reaches the kernel as a can_use_tool request, because the
// settings below allow file tools and never Bash.
package claudecli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/panca/haythos/app"
)

const (
	AllowOption = "allow"
	DenyOption  = "deny"

	initTimeout   = 30 * time.Second
	interruptWait = 5 * time.Second
	denyMessage   = "denied by HaythOS"
)

// Config is one claude runtime. The child environment is PATH, HOME, USER, HAYTHOS_TASK, HAYTHOS_SESSION and PassEnv; nothing else.
type Config struct {
	Command string
	// Args are prepended to every invocation (tests use them to re-exec a fake).
	Args []string
	// Model is the default when RuntimeStart names none.
	Model     string
	ExtraArgs []string
	PassEnv   []string
	// SettingSources defaults to "project,local". User settings stay out: the owner's ~/.claude carries allow rules, hooks and
	// defaultMode auto, any of which would answer a Bash call before it becomes a can_use_tool request.
	SettingSources string
	// Allow and Ask replace the default permission rules; never put Bash in Allow.
	Allow, Ask []string
}

var (
	defaultAllow = []string{"Read", "Edit", "Write", "Glob", "Grep", "NotebookEdit"}
	defaultAsk   = credentialAsk()
)

// credentialAsk also guards the runtimes' own config: a planted settings.local.json could grant itself a Bash allow rule.
func credentialAsk() []string {
	var rules []string
	for _, tool := range []string{"Read", "Grep", "Glob"} {
		for _, p := range []string{"**/.env*", "~/.ssh/**", "~/.aws/**", "~/.netrc", "~/.git-credentials", "~/.gnupg/**"} {
			rules = append(rules, tool+"("+p+")")
		}
	}
	for _, tool := range []string{"Edit", "Write"} {
		for _, p := range []string{"**/.claude/**", "**/.codex/**", "**/opencode.json"} {
			rules = append(rules, tool+"("+p+")")
		}
	}
	return rules
}

type Runtime struct{ cfg Config }

func New(cfg Config) *Runtime {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	if cfg.SettingSources == "" {
		cfg.SettingSources = "project,local"
	}
	if cfg.Allow == nil {
		cfg.Allow = defaultAllow
	}
	if cfg.Ask == nil {
		cfg.Ask = defaultAsk
	}
	return &Runtime{cfg: cfg}
}

func (r *Runtime) Name() string { return "claude" }

func (r *Runtime) Capabilities() app.RuntimeCaps {
	return app.RuntimeCaps{Resume: true, Modes: []string{"default"}}
}

func (r *Runtime) settings() (string, error) {
	b, err := json.Marshal(map[string]any{"permissions": map[string]any{"defaultMode": "default", "allow": r.cfg.Allow, "ask": r.cfg.Ask}})
	if err != nil {
		return "", fmt.Errorf("encode claude settings: %w", err)
	}
	return string(b), nil
}

func (r *Runtime) Start(ctx context.Context, req app.RuntimeStart) (app.RuntimeSession, error) {
	if req.Mode != "" && req.Mode != "default" {
		return nil, fmt.Errorf("claude runtime: mode %q is not supported", req.Mode)
	}
	if slices.Contains(r.cfg.Allow, "Bash") {
		return nil, errors.New("claude runtime: Bash must not be allowed, every shell command goes through the guard")
	}
	settings, err := r.settings()
	if err != nil {
		return nil, err
	}
	ref := req.ResumeRef
	args := slices.Clone(r.cfg.Args)
	args = append(args, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--permission-prompt-tool", "stdio",
		"--setting-sources", r.cfg.SettingSources, "--settings", settings)
	if ref != "" {
		args = append(args, "--resume", ref)
	} else {
		ref = newUUID()
		args = append(args, "--session-id", ref)
	}
	if model := cmpOr(req.Model, r.cfg.Model); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, r.cfg.ExtraArgs...)

	cmd := exec.CommandContext(context.WithoutCancel(ctx), r.cfg.Command, args...) //nolint:gosec // command comes from config, args are fixed flags
	cmd.Dir = req.Workdir
	cmd.Env = r.env(req)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout: %w", err)
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", r.cfg.Command, err)
	}
	s := &session{
		cmd: cmd, stdin: stdin, stderr: &stderr, ref: ref, events: make(chan app.RuntimeEvent, 256),
		turn: make(chan turnResult, 1), exited: make(chan struct{}), stopped: make(chan struct{}),
		pending: map[string]json.RawMessage{}, waiters: map[string]chan frame{},
	}
	go s.read(stdout)
	initCtx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if err := s.control(initCtx, "initialize"); err != nil {
		_ = s.Stop(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("initialize claude: %w", err)
	}
	slog.InfoContext(ctx, "claude session started", "task", req.TaskID, "session", req.SessionID)
	return s, nil
}

func (r *Runtime) env(req app.RuntimeStart) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER"), "HAYTHOS_TASK=" + req.TaskID, "HAYTHOS_SESSION=" + req.SessionID}
	for _, name := range r.cfg.PassEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type frame struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	RequestID string          `json:"request_id"`
	Message   json.RawMessage `json:"message"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`

	IsError        bool   `json:"is_error"`
	StopReason     string `json:"stop_reason"`
	TerminalReason string `json:"terminal_reason"`
}

type turnResult struct {
	stop app.StopReason
	err  error
}

type session struct {
	cmd    *exec.Cmd
	stderr *tailBuffer
	events chan app.RuntimeEvent
	turn   chan turnResult
	// exited closes when the reader is done and events is closed; stopped closes at the start of Stop.
	exited, stopped chan struct{}
	stopOnce        sync.Once
	stopErr         error

	writeMu sync.Mutex
	stdin   io.WriteCloser

	mu      sync.Mutex
	ref     string
	seq     int
	busy    bool
	pending map[string]json.RawMessage
	waiters map[string]chan frame
}

func (s *session) Ref() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ref
}

func (s *session) Events() <-chan app.RuntimeEvent { return s.events }

func (s *session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write to claude: %w", err)
	}
	return nil
}

// control sends a control request and waits for its control_response.
func (s *session) control(ctx context.Context, subtype string) error {
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("haythos-%d", s.seq)
	ch := make(chan frame, 1)
	s.waiters[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters, id)
		s.mu.Unlock()
	}()
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{"subtype": subtype}}); err != nil {
		return err
	}
	select {
	case f := <-ch:
		var resp struct {
			Subtype string `json:"subtype"`
			Error   string `json:"error"`
		}
		if err := json.Unmarshal(f.Response, &resp); err != nil {
			return fmt.Errorf("decode control response: %w", err)
		}
		if resp.Subtype != "success" {
			return fmt.Errorf("claude rejected %s: %s", subtype, resp.Error)
		}
		return nil
	case <-s.exited:
		return s.exitError()
	case <-ctx.Done():
		return fmt.Errorf("await %s response: %w", subtype, ctx.Err())
	}
}

func (s *session) exitError() error {
	return fmt.Errorf("claude exited: %s", s.stderr.String())
}

func (s *session) Send(ctx context.Context, prompt string) (app.StopReason, error) {
	select {
	case <-s.stopped:
		return "", errors.New("send: session stopped")
	case <-s.exited:
		return "", s.exitError()
	default:
	}
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return "", errors.New("send: a turn is already running")
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()
	msg := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}}
	if err := s.write(map[string]any{"type": "user", "message": msg}); err != nil {
		return "", err
	}
	select {
	case res := <-s.turn:
		return res.stop, res.err
	case <-s.exited:
		select {
		case res := <-s.turn:
			return res.stop, res.err
		default:
		}
		select {
		case <-s.stopped:
			return app.StopCancelled, nil
		default:
			return "", s.exitError()
		}
	case <-ctx.Done():
		return "", fmt.Errorf("send: %w", ctx.Err())
	}
}

func (s *session) Approve(_ context.Context, requestID, optionID string) error {
	s.mu.Lock()
	input, ok := s.pending[requestID]
	delete(s.pending, requestID)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("approve %s: no such pending request", requestID)
	}
	resp := map[string]any{"behavior": "deny", "message": denyMessage}
	if optionID == AllowOption {
		resp = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	return s.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": requestID, "response": resp}})
}

// Stop interrupts a running turn, then kills the process group; calling it again returns the first result.
func (s *session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		busy := s.busy
		s.mu.Unlock()
		if busy {
			ictx, cancel := context.WithTimeout(ctx, interruptWait)
			_ = s.control(ictx, "interrupt")
			cancel()
			select {
			case <-s.exited:
			case <-time.After(100 * time.Millisecond):
			}
		}
		close(s.stopped)
		_ = s.stdin.Close()
		select {
		case <-s.exited:
		default:
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-s.exited:
		case <-ctx.Done():
			s.stopErr = fmt.Errorf("stop claude: %w", ctx.Err())
		}
	})
	return s.stopErr
}

func (s *session) emit(ev app.RuntimeEvent) {
	select {
	case s.events <- ev:
	case <-s.stopped:
	}
}

func (s *session) read(stdout io.Reader) {
	defer func() {
		_ = s.cmd.Wait()
		close(s.events)
		close(s.exited)
	}()
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var f frame
		if json.Unmarshal(sc.Bytes(), &f) != nil {
			continue
		}
		s.handle(f)
	}
	if err := sc.Err(); err != nil {
		slog.Warn("claude stdout", "err", err)
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (s *session) handle(f frame) {
	switch f.Type {
	case "system":
		if f.Subtype == "init" && f.SessionID != "" {
			s.mu.Lock()
			s.ref = f.SessionID
			s.mu.Unlock()
		}
	case "assistant":
		s.assistant(f.Message)
	case "user":
		s.toolResults(f.Message)
	case "control_request":
		s.controlRequest(f)
	case "control_response":
		var r struct {
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(f.Response, &r)
		s.mu.Lock()
		ch := s.waiters[r.RequestID]
		s.mu.Unlock()
		if ch != nil {
			ch <- f
		}
	case "result":
		stop, err := stopReason(f)
		s.emit(app.RuntimeEvent{Kind: app.RuntimeStop, Stop: stop})
		s.turn <- turnResult{stop, err}
	}
}

func stopReason(f frame) (app.StopReason, error) {
	switch {
	case strings.HasPrefix(f.TerminalReason, "aborted"):
		return app.StopCancelled, nil
	case f.Subtype == "error_max_turns":
		return app.StopMaxTurnRequests, nil
	case f.IsError || f.Subtype != "success":
		return "", fmt.Errorf("claude turn failed: subtype=%s terminal_reason=%s", f.Subtype, f.TerminalReason)
	case f.StopReason == "max_tokens":
		return app.StopMaxTokens, nil
	case f.StopReason == "refusal":
		return app.StopRefusal, nil
	}
	return app.StopEndTurn, nil
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

type message struct {
	ID      string  `json:"id"`
	Content []block `json:"content"`
}

func (s *session) assistant(raw json.RawMessage) {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			s.emit(app.RuntimeEvent{Kind: app.RuntimeMessage, ID: m.ID, Text: b.Text})
		case "thinking":
			s.emit(app.RuntimeEvent{Kind: app.RuntimeThought, ID: m.ID, Text: b.Thinking})
		case "tool_use":
			kind, text := toolKind(b.Name, b.Input)
			s.emit(app.RuntimeEvent{Kind: app.RuntimeToolCall, ID: b.ID, ToolKind: kind, Text: text, Status: "pending"})
		}
	}
}

func (s *session) toolResults(raw json.RawMessage) {
	var m message
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for _, b := range m.Content {
		if b.Type != "tool_result" {
			continue
		}
		status := "completed"
		if b.IsError {
			status = "failed"
		}
		s.emit(app.RuntimeEvent{Kind: app.RuntimeToolCallUpdate, ID: b.ToolUseID, Status: status})
	}
}

func (s *session) controlRequest(f frame) {
	var req struct {
		Subtype  string          `json:"subtype"`
		ToolName string          `json:"tool_name"`
		Input    json.RawMessage `json:"input"`
	}
	if json.Unmarshal(f.Request, &req) != nil || req.Subtype != "can_use_tool" {
		resp := map[string]any{"subtype": "error", "request_id": f.RequestID, "error": "unsupported control request"}
		_ = s.write(map[string]any{"type": "control_response", "response": resp})
		return
	}
	kind, text := toolKind(req.ToolName, req.Input)
	s.mu.Lock()
	s.pending[f.RequestID] = req.Input
	s.mu.Unlock()
	s.emit(app.RuntimeEvent{Kind: app.RuntimePermissionRequest, Permission: &app.PermissionRequest{
		RequestID: f.RequestID, Tool: req.ToolName, Kind: kind, Text: text, AllowOptionID: AllowOption, RejectOptionID: DenyOption,
	}})
}

// toolKind maps a Claude Code tool to an ACP tool kind and the text the guard matches: the command, path, url or pattern.
func toolKind(name string, input json.RawMessage) (kind, text string) {
	var in struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
		URL      string `json:"url"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "Bash":
		return "execute", in.Command
	case "Read":
		return "read", in.FilePath
	case "Edit", "Write", "NotebookEdit":
		return "edit", cmpOr(in.FilePath, in.Path)
	case "Glob", "Grep":
		return "read", strings.TrimSpace(in.Path + " " + in.Pattern)
	case "WebFetch":
		return "fetch", in.URL
	}
	return "other", name
}

type tailBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

const tailMax = 500

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b.Write(p)
	if over := t.b.Len() - 4*tailMax; over > 0 {
		t.b.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := strings.TrimSpace(t.b.String())
	if len(s) > tailMax {
		s = s[len(s)-tailMax:]
	}
	return s
}
