package claudesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRecord struct {
	Args  []string `json:"args"`
	Env   []string `json:"env"`
	Stdin string   `json:"stdin"`
	Dir   string   `json:"dir"`
}

// TestCLIHelper is a fake claude CLI driven by CLI_FAKE; it records argv, env, stdin and cwd to CLI_FAKE_RECORD.
func TestCLIHelper(t *testing.T) {
	mode := os.Getenv("CLI_FAKE")
	if mode == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	dir, _ := os.Getwd()
	rec := fakeRecord{Args: args, Env: os.Environ(), Dir: dir}
	if mode == "models" || mode == "models-silent" {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		rec.Stdin = line
	} else {
		b, _ := bufio.NewReader(os.Stdin).ReadString(0)
		rec.Stdin = b
	}
	raw, _ := json.Marshal(rec)
	_ = os.WriteFile(os.Getenv("CLI_FAKE_RECORD"), raw, 0o600)
	switch mode {
	case "structured":
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"text":"hi"},"usage":{"input_tokens":3,"output_tokens":2}}`)
	case "plain":
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"plain","usage":{"input_tokens":1,"output_tokens":1}}`)
	case "error":
		fmt.Println(`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key · Please run /login"}`)
		os.Exit(1)
	case "crash":
		fmt.Fprintln(os.Stderr, "boom: native binary missing")
		os.Exit(2)
	case "sleep":
		time.Sleep(10 * time.Second)
	case "models":
		fmt.Println(`{"type":"system","subtype":"init"}`)
		fmt.Println(`{"type":"control_response","response":{"subtype":"success","request_id":"models","response":{"models":[{"value":"opus","displayName":"Opus","description":"Best"},{"value":"haiku","displayName":"Haiku","description":"Fast"}]}}}`)
		time.Sleep(10 * time.Second)
	case "models-silent":
		time.Sleep(10 * time.Second)
	}
	os.Exit(0)
}

func fakeCLI(t *testing.T, mode string) (*CLI, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record.json")
	c := NewCLI(CLIConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestCLIHelper", "--"},
		Env:     []string{"CLI_FAKE=" + mode, "CLI_FAKE_RECORD=" + record},
	})
	return c, record
}

func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake never ran: %v", err)
	}
	var rec fakeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestCLIStepArgsAndEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
	}{{"with model", "sonnet"}, {"default model", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			c, record := fakeCLI(t, "structured")
			req := StepRequest{System: "be terse", Prompt: "hello\nworld", Schema: json.RawMessage(`{"type":"object"}`), Tools: []string{}, Model: tc.model}
			if _, err := c.Step(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			rec := readRecord(t, record)
			want := []string{"-p", "--output-format", "json", "--tools", "", "--setting-sources", "", "--no-session-persistence",
				"--strict-mcp-config", "--max-turns", "2", "--system-prompt", "be terse", "--json-schema", `{"type":"object"}`}
			if tc.model != "" {
				want = append(want, "--model", tc.model)
			}
			if !slices.Equal(rec.Args, want) {
				t.Errorf("args =\n%q\nwant\n%q", rec.Args, want)
			}
			if rec.Stdin != req.Prompt {
				t.Errorf("stdin = %q", rec.Stdin)
			}
			wd, _ := os.Getwd()
			if rec.Dir == wd {
				t.Error("CLI ran in the caller's cwd, want a fresh temp dir")
			}
			if _, err := os.Stat(rec.Dir); !os.IsNotExist(err) {
				t.Errorf("temp dir %s not removed", rec.Dir)
			}
			for _, kv := range []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_MAX_RETRIES=0"} {
				if !slices.Contains(rec.Env, kv) {
					t.Errorf("env missing %s", kv)
				}
			}
			for _, kv := range rec.Env {
				name, _, _ := strings.Cut(kv, "=")
				if !slices.Contains([]string{"CLI_FAKE", "CLI_FAKE_RECORD", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CODE_MAX_RETRIES", "__CF_USER_TEXT_ENCODING"}, name) {
					t.Errorf("unexpected inherited env %s", name)
				}
			}
		})
	}
}

func TestCLIStepParsesStructuredOutput(t *testing.T) {
	c, _ := fakeCLI(t, "structured")
	resp, err := c.Step(context.Background(), StepRequest{Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"text":"hi"}` || resp.Usage.InputTokens != 3 || resp.Usage.OutputTokens != 2 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestCLIStepFallsBackToResult(t *testing.T) {
	c, _ := fakeCLI(t, "plain")
	resp, err := c.Step(context.Background(), StepRequest{Tools: []string{}})
	if err != nil || resp.Text != "plain" {
		t.Errorf("resp = %+v err = %v", resp, err)
	}
}

func TestCLIStepErrors(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"error", "Invalid API key"},
		{"crash", "boom: native binary missing"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c, _ := fakeCLI(t, tc.mode)
			_, err := c.Step(context.Background(), StepRequest{Tools: []string{}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCLIStepRejectsBuiltinTools(t *testing.T) {
	c, _ := fakeCLI(t, "structured")
	if _, err := c.Step(context.Background(), StepRequest{Tools: []string{"Bash"}}); err == nil {
		t.Error("want error when SDK built-in tools are requested")
	}
}

func TestCLIStepMissingBinary(t *testing.T) {
	c := NewCLI(CLIConfig{Command: "/nonexistent/claude"})
	if _, err := c.Step(context.Background(), StepRequest{Tools: []string{}}); err == nil || !strings.Contains(err.Error(), "/nonexistent/claude") {
		t.Errorf("err = %v", err)
	}
}

func TestCLIStepCancel(t *testing.T) {
	c, _ := fakeCLI(t, "sleep")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Step(ctx, StepRequest{Tools: []string{}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("Step took %v after cancel", time.Since(start))
	}
}

func TestCLIModels(t *testing.T) {
	c, record := fakeCLI(t, "models")
	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelInfo{{Value: "opus", DisplayName: "Opus", Description: "Best"}, {Value: "haiku", DisplayName: "Haiku", Description: "Fast"}}
	if !slices.Equal(got, want) {
		t.Errorf("models = %+v", got)
	}
	rec := readRecord(t, record)
	if strings.TrimSpace(rec.Stdin) != `{"type":"control_request","request_id":"models","request":{"subtype":"initialize"}}` {
		t.Errorf("stdin = %q", rec.Stdin)
	}
	for _, flag := range []string{"--input-format", "--output-format", "--verbose", "--strict-mcp-config"} {
		if !slices.Contains(rec.Args, flag) {
			t.Errorf("args missing %s: %q", flag, rec.Args)
		}
	}
	if slices.Contains(rec.Args, "--json-schema") {
		t.Error("model list must not send a model turn")
	}
}

func TestCLIModelsNoResponse(t *testing.T) {
	c, _ := fakeCLI(t, "models-silent")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := c.Models(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
}
