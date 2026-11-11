package claudesdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/panca/haythos/core/task"
)

// CLIConfig runs the official claude CLI. Env is the complete child environment; it is never inherited.
type CLIConfig struct {
	Command string
	// Args are prepended to every invocation (used by tests to re-exec a fake).
	Args []string
	Env  []string
	// StateDir holds the CLI's own config, session transcripts and working directory; empty means a temp dir.
	StateDir string
}

const maxSessions = 8

// CLI spawns one `claude -p` process per call; a prompt that extends an earlier one resumes that CLI session, the only shape its prompt cache hits.
type CLI struct {
	cfg CLIConfig

	prepare  sync.Once
	prepErr  error
	work     string
	config   string
	mu       sync.Mutex
	sessions []session
}

type session struct {
	id, key string
	covered [sha256.Size]byte
	n       int
}

func NewCLI(cfg CLIConfig) *CLI {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	return &CLI{cfg: cfg}
}

// isolationEnv keeps Claude Code's own memory, CLAUDE.md files and retries out of a kernel step.
var isolationEnv = []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_MAX_RETRIES=0"}

var isolationArgs = []string{"--tools", "", "--setting-sources", "", "--no-session-persistence", "--strict-mcp-config"}

var stepArgs = []string{"-p", "--output-format", "json", "--tools", "", "--setting-sources", "", "--strict-mcp-config", "--max-turns", "2"}

type cliResult struct {
	IsError          bool            `json:"is_error"`
	Subtype          string          `json:"subtype"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	// Cached prompt tokens are reported apart from input_tokens; they are still context the model read.
	Usage struct {
		InputTokens   int `json:"input_tokens"`
		CacheCreation int `json:"cache_creation_input_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
		OutputTokens  int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *CLI) command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.cfg.Command, append(slices.Clone(c.cfg.Args), args...)...) //nolint:gosec // command comes from config, args are fixed flags
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(c.cfg.Env), isolationEnv...)
	return cmd
}

func (c *CLI) Step(ctx context.Context, req StepRequest) (StepResponse, error) {
	if len(req.Tools) != 0 {
		return StepResponse{}, errors.New("claude cli: built-in tools are never enabled")
	}
	if c.prepare.Do(c.prepareState); c.prepErr != nil {
		return StepResponse{}, c.prepErr
	}
	key := req.System + "\x00" + req.Model + "\x00" + string(req.Schema)
	if s, ok := c.take(key, req.Prompt); ok {
		resp, err := c.run(ctx, req, "--resume", s.id, req.Prompt[s.n:])
		if err == nil {
			c.put(s.id, key, req.Prompt)
			return resp, nil
		}
		c.forget(s.id)
		if ctx.Err() != nil {
			return StepResponse{}, err
		}
		slog.WarnContext(ctx, "claude cli session could not be resumed; starting a new one", "err", err)
	}
	id := newSessionID()
	resp, err := c.run(ctx, req, "--session-id", id, req.Prompt)
	if err != nil {
		c.forget(id)
		return StepResponse{}, err
	}
	c.put(id, key, req.Prompt)
	return resp, nil
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (c *CLI) prepareState() {
	root := c.cfg.StateDir
	if root == "" {
		if root, c.prepErr = os.MkdirTemp("", "haythos-claude-"); c.prepErr != nil {
			c.prepErr = fmt.Errorf("claude cli state dir: %w", c.prepErr)
			return
		}
	}
	c.work, c.config = filepath.Join(root, "work"), filepath.Join(root, "config")
	if err := os.RemoveAll(filepath.Join(c.config, "projects")); err != nil {
		c.prepErr = fmt.Errorf("clear claude cli transcripts: %w", err)
		return
	}
	for _, dir := range []string{c.work, c.config} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			c.prepErr = fmt.Errorf("claude cli state dir: %w", err)
			return
		}
	}
}

// take removes and returns the session that covers the longest proper prefix of prompt, so no two steps resume it at once.
func (c *CLI) take(key, prompt string) (session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	best := -1
	for i, s := range c.sessions {
		if s.key == key && s.n < len(prompt) && (best < 0 || s.n > c.sessions[best].n) && sha256.Sum256([]byte(prompt[:s.n])) == s.covered {
			best = i
		}
	}
	if best < 0 {
		return session{}, false
	}
	s := c.sessions[best]
	c.sessions = slices.Delete(c.sessions, best, best+1)
	return s, true
}

func (c *CLI) put(id, key, prompt string) {
	c.mu.Lock()
	c.sessions = append(c.sessions, session{id: id, key: key, covered: sha256.Sum256([]byte(prompt)), n: len(prompt)})
	var dropped []session
	if extra := len(c.sessions) - maxSessions; extra > 0 {
		dropped = slices.Clone(c.sessions[:extra])
		c.sessions = slices.Delete(c.sessions, 0, extra)
	}
	c.mu.Unlock()
	for _, s := range dropped {
		c.forget(s.id)
	}
}

// forget deletes a session's transcript; the CLI keeps it at <config>/projects/<cwd slug>/<id>.jsonl.
func (c *CLI) forget(id string) {
	paths, _ := filepath.Glob(filepath.Join(c.config, "projects", "*", id+".jsonl"))
	for _, p := range paths {
		_ = os.Remove(p)
	}
}

func (c *CLI) run(ctx context.Context, req StepRequest, sessionFlag, id, stdin string) (StepResponse, error) {
	args := append(slices.Clone(stepArgs), "--system-prompt", req.System, "--json-schema", string(req.Schema))
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	cmd := c.command(ctx, c.work, append(args, sessionFlag, id)...)
	cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+c.config)
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	if ctx.Err() != nil {
		return StepResponse{}, fmt.Errorf("claude cli step: %w", ctx.Err())
	}
	var res cliResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		if runErr != nil {
			slog.DebugContext(ctx, "claude cli stderr", "tail", tail(stderr.String()))
			return StepResponse{}, fmt.Errorf("run %s: %w", c.cfg.Command, runErr)
		}
		return StepResponse{}, fmt.Errorf("decode claude cli output: %w", err)
	}
	if res.IsError || runErr != nil {
		slog.DebugContext(ctx, "claude cli failure", "tail", tail(res.Result))
		exit := 0
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exit = ee.ExitCode()
		}
		return StepResponse{}, fmt.Errorf("claude cli step failed: is_error=%t subtype=%q exit=%d", res.IsError, res.Subtype, exit)
	}
	text := res.Result
	if len(res.StructuredOutput) > 0 && string(res.StructuredOutput) != "null" {
		text = string(res.StructuredOutput)
	}
	return StepResponse{Text: text, Usage: task.Usage{InputTokens: res.Usage.InputTokens + res.Usage.CacheCreation + res.Usage.CacheRead, OutputTokens: res.Usage.OutputTokens, CacheReadTokens: res.Usage.CacheRead}}, nil
}

// Models asks the CLI for its live model list through the stream-json initialize handshake; no model turn is sent.
func (c *CLI) Models(ctx context.Context) ([]ModelInfo, error) {
	dir, err := os.MkdirTemp("", "haythos-claude-")
	if err != nil {
		return nil, fmt.Errorf("claude cli temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := append([]string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}, isolationArgs...)
	cmd := c.command(ctx, dir, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude cli stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude cli stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.cfg.Command, err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	if _, err := io.WriteString(stdin, `{"type":"control_request","request_id":"models","request":{"subtype":"initialize"}}`+"\n"); err != nil {
		return nil, fmt.Errorf("send initialize: %w", err)
	}
	found := make(chan []ModelInfo, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var ev struct {
				Type     string `json:"type"`
				Response struct {
					RequestID string `json:"request_id"`
					Response  struct {
						Models []ModelInfo `json:"models"`
					} `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == "control_response" && ev.Response.RequestID == "models" {
				found <- ev.Response.Response.Models
				return
			}
		}
		close(found)
	}()
	select {
	case models, ok := <-found:
		if !ok {
			return nil, errors.New("claude cli exited without a model list")
		}
		return models, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("claude cli models: %w", ctx.Err())
	}
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[len(s)-500:]
	}
	return s
}
