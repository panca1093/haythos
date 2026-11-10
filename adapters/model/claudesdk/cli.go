package claudesdk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/panca/haythos/core/task"
)

// CLIConfig runs the official claude CLI. Env is the complete child environment; it is never inherited.
type CLIConfig struct {
	Command string
	// Args are prepended to every invocation (used by tests to re-exec a fake).
	Args []string
	Env  []string
}

// CLI spawns one `claude -p` process per call; it holds no state between calls.
type CLI struct{ cfg CLIConfig }

func NewCLI(cfg CLIConfig) *CLI {
	if cfg.Command == "" {
		cfg.Command = "claude"
	}
	return &CLI{cfg: cfg}
}

// isolationEnv keeps Claude Code's own memory, CLAUDE.md files and retries out of a kernel step.
var isolationEnv = []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_MAX_RETRIES=0"}

var isolationArgs = []string{"--tools", "", "--setting-sources", "", "--no-session-persistence", "--strict-mcp-config"}

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
	dir, err := os.MkdirTemp("", "haythos-claude-")
	if err != nil {
		return StepResponse{}, fmt.Errorf("claude cli temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	args := append([]string{"-p", "--output-format", "json"}, isolationArgs...)
	args = append(args, "--max-turns", "2", "--system-prompt", req.System, "--json-schema", string(req.Schema))
	if req.Model != "" {
		args = append(args, "--model", req.Model)
	}
	cmd := c.command(ctx, dir, args...)
	cmd.Stdin = strings.NewReader(req.Prompt)
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
	return StepResponse{Text: text, Usage: task.Usage{InputTokens: res.Usage.InputTokens + res.Usage.CacheCreation + res.Usage.CacheRead, OutputTokens: res.Usage.OutputTokens}}, nil
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
