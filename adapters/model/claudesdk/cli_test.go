package claudesdk

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/core/task"
)

type fakeRecord struct {
	Args  []string `json:"args"`
	Env   []string `json:"env"`
	Stdin string   `json:"stdin"`
	Dir   string   `json:"dir"`
}

// TestCLIHelper is a fake claude CLI driven by CLI_FAKE; it appends argv, env, stdin and cwd to CLI_FAKE_RECORD, one JSON line per run.
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
	if f, err := os.OpenFile(os.Getenv("CLI_FAKE_RECORD"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		_, _ = f.Write(append(raw, '\n'))
		_ = f.Close()
	}
	if flag, id := sessionFlag(args); flag == "--session-id" {
		p := fakeTranscript(filepath.Dir(os.Getenv("CLAUDE_CONFIG_DIR")), id)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		_ = os.WriteFile(p, []byte("{}\n"), 0o600)
	}
	if flag, id := sessionFlag(args); mode == "resume-fails" || mode == "resume-overloaded" {
		if flag == "--resume" {
			if mode == "resume-fails" {
				fmt.Fprintln(os.Stderr, "No conversation found with session ID: "+id)
			} else {
				fmt.Fprintln(os.Stderr, "API Error: 529 overloaded")
			}
			os.Exit(1)
		}
		mode = "structured"
	}
	switch mode {
	case "structured":
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"text":"hi"},"usage":{"input_tokens":3,"output_tokens":2}}`)
	case "cached":
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"ok","usage":{"input_tokens":2,"cache_creation_input_tokens":1458,"cache_read_input_tokens":9000,"output_tokens":53}}`)
	case "stream":
		if slices.Contains(args, "--input-format") {
			fmt.Println(`{"type":"system","subtype":"init"}`)
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"thinking"}]}}`)
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"text":"red"},"usage":{"input_tokens":3,"output_tokens":2}}`)
		} else {
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"text":"plain"},"usage":{"input_tokens":3,"output_tokens":2}}`)
		}
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
	return fakeCLIIn(t, mode, t.TempDir())
}

func fakeCLIIn(t *testing.T, mode, state string) (*CLI, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record.json")
	c := NewCLI(CLIConfig{
		Command:  os.Args[0],
		Args:     []string{"-test.run=TestCLIHelper", "--"},
		Env:      []string{"CLI_FAKE=" + mode, "CLI_FAKE_RECORD=" + record},
		StateDir: state,
	})
	return c, record
}

func readRecords(t *testing.T, path string) []fakeRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fake never ran: %v", err)
	}
	var recs []fakeRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec fakeRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		recs = append(recs, rec)
	}
	return recs
}

func readRecord(t *testing.T, path string) fakeRecord {
	t.Helper()
	recs := readRecords(t, path)
	return recs[len(recs)-1]
}

func sessionFlag(args []string) (flag, id string) {
	for _, f := range []string{"--session-id", "--resume"} {
		if i := slices.Index(args, f); i >= 0 && i+1 < len(args) {
			return f, args[i+1]
		}
	}
	return "", ""
}

func fakeTranscript(state, id string) string {
	return filepath.Join(state, "config", "projects", "fake", id+".jsonl")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func step(t *testing.T, c *CLI, req StepRequest) {
	t.Helper()
	if _, err := c.Step(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestCLIStepArgsAndEnv(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
	}{{"with model", "sonnet"}, {"default model", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			c, record := fakeCLIIn(t, "structured", state)
			req := StepRequest{System: "be terse", Prompt: "hello\nworld", Schema: json.RawMessage(`{"type":"object"}`), Tools: []string{}, Model: tc.model}
			step(t, c, req)
			rec := readRecord(t, record)
			want := []string{"-p", "--output-format", "json", "--tools", "", "--setting-sources", "", "--strict-mcp-config",
				"--max-turns", "2", "--system-prompt", "be terse", "--json-schema", `{"type":"object"}`}
			if tc.model != "" {
				want = append(want, "--model", tc.model)
			}
			n := len(rec.Args)
			if n < 2 || !slices.Equal(rec.Args[:n-2], want) || rec.Args[n-2] != "--session-id" || !uuidRe.MatchString(rec.Args[n-1]) {
				t.Errorf("args =\n%q\nwant\n%q followed by --session-id <uuid>", rec.Args, want)
			}
			if rec.Stdin != req.Prompt {
				t.Errorf("stdin = %q", rec.Stdin)
			}
			if rec.Dir != filepath.Join(state, "work") {
				t.Errorf("cwd = %s, want the fixed work dir in %s", rec.Dir, state)
			}
			if !slices.Contains(rec.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(state, "config")) {
				t.Errorf("env does not point the CLI's config at the state dir: %q", rec.Env)
			}
			for _, kv := range []string{"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_MAX_RETRIES=0"} {
				if !slices.Contains(rec.Env, kv) {
					t.Errorf("env missing %s", kv)
				}
			}
			for _, kv := range rec.Env {
				name, _, _ := strings.Cut(kv, "=")
				if !slices.Contains([]string{"CLI_FAKE", "CLI_FAKE_RECORD", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "CLAUDE_CODE_DISABLE_CLAUDE_MDS", "CLAUDE_CODE_MAX_RETRIES", "__CF_USER_TEXT_ENCODING"}, name) {
					t.Errorf("unexpected inherited env %s", name)
				}
			}
		})
	}
}

func TestCLIStepResumesWhenPromptExtends(t *testing.T) {
	t.Parallel()
	c, record := fakeCLI(t, "structured")
	req := StepRequest{System: "s", Prompt: "tools\n[user] hi\n", Schema: json.RawMessage(`{}`), Tools: []string{}}
	step(t, c, req)
	added := []string{"[assistant] yo\n[tool result id=1] ok\n", "[user] again\n"}
	for _, a := range added {
		req.Prompt += a
		step(t, c, req)
	}
	recs := readRecords(t, record)
	if len(recs) != 3 {
		t.Fatalf("CLI ran %d times, want 3", len(recs))
	}
	_, id := sessionFlag(recs[0].Args)
	for i, a := range added {
		rec := recs[i+1]
		if flag, got := sessionFlag(rec.Args); flag != "--resume" || got != id {
			t.Errorf("step %d: %s %s, want --resume %s", i+2, flag, got, id)
		}
		if rec.Stdin != a {
			t.Errorf("step %d: stdin = %q, want only the new text %q", i+2, rec.Stdin, a)
		}
		if rec.Dir != recs[0].Dir {
			t.Errorf("step %d: cwd = %s, want %s", i+2, rec.Dir, recs[0].Dir)
		}
	}
}

func TestCLIStepStartsNewSessionWhenNotAnExtension(t *testing.T) {
	base := StepRequest{System: "s", Prompt: "one\n", Schema: json.RawMessage(`{"a":1}`), Tools: []string{}, Model: "m"}
	for _, tc := range []struct {
		name string
		next func(StepRequest) StepRequest
	}{
		{"different prompt", func(r StepRequest) StepRequest { r.Prompt = "two\nthree\n"; return r }},
		{"same prompt again", func(r StepRequest) StepRequest { return r }},
		{"system changed", func(r StepRequest) StepRequest { r.System, r.Prompt = "t", r.Prompt+"x"; return r }},
		{"model changed", func(r StepRequest) StepRequest { r.Model, r.Prompt = "n", r.Prompt+"x"; return r }},
		{"schema changed", func(r StepRequest) StepRequest {
			r.Schema, r.Prompt = json.RawMessage(`{"a":2}`), r.Prompt+"x"
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, record := fakeCLI(t, "structured")
			step(t, c, base)
			next := tc.next(base)
			step(t, c, next)
			recs := readRecords(t, record)
			_, first := sessionFlag(recs[0].Args)
			flag, id := sessionFlag(recs[1].Args)
			if flag != "--session-id" || id == first || !uuidRe.MatchString(id) {
				t.Errorf("second run used %s %s (first session %s), want a new session", flag, id, first)
			}
			if recs[1].Stdin != next.Prompt {
				t.Errorf("stdin = %q, want the full prompt", recs[1].Stdin)
			}
		})
	}
}

func TestCLIStepResumeFailureStartsFresh(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	c, record := fakeCLIIn(t, "resume-fails", state)
	req := StepRequest{Prompt: "one\n", Tools: []string{}}
	step(t, c, req)
	req.Prompt += "two\n"
	resp, err := c.Step(context.Background(), req)
	if err != nil || resp.Text != `{"text":"hi"}` {
		t.Fatalf("resp = %+v err = %v, want the fresh session's answer", resp, err)
	}
	recs := readRecords(t, record)
	if len(recs) != 3 {
		t.Fatalf("CLI ran %d times, want 3 (new, failed resume, new)", len(recs))
	}
	_, first := sessionFlag(recs[0].Args)
	if flag, _ := sessionFlag(recs[1].Args); flag != "--resume" {
		t.Errorf("second run used %s, want --resume", flag)
	}
	if flag, id := sessionFlag(recs[2].Args); flag != "--session-id" || id == first || recs[2].Stdin != req.Prompt {
		t.Errorf("third run used %s %s with stdin %q, want a new session with the full prompt", flag, id, recs[2].Stdin)
	}
	if exists(fakeTranscript(state, first)) {
		t.Error("transcript of the session that could not be resumed was kept")
	}
}

func TestCLIStepResumeErrorOtherThanMissingSessionIsNotRetried(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	c, record := fakeCLIIn(t, "resume-overloaded", state)
	req := StepRequest{Prompt: "one\n", Tools: []string{}}
	step(t, c, req)
	req.Prompt += "two\n"
	if _, err := c.Step(context.Background(), req); err == nil {
		t.Fatal("want the overload error, not a second full-price call")
	}
	recs := readRecords(t, record)
	if len(recs) != 2 {
		t.Fatalf("CLI ran %d times, want 2 (new, failed resume)", len(recs))
	}
	_, first := sessionFlag(recs[0].Args)
	if exists(fakeTranscript(state, first)) {
		t.Error("transcript of a session whose resume failed was kept")
	}
}

func TestCLIStepMakesARelativeStateDirAbsolute(t *testing.T) {
	t.Parallel()
	abs, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	rel, err := filepath.Rel(wd, abs)
	if err != nil || filepath.IsAbs(rel) {
		t.Skipf("no relative form of %s from %s", abs, wd)
	}
	c, record := fakeCLIIn(t, "structured", rel)
	step(t, c, StepRequest{Prompt: "one\n", Tools: []string{}})
	rec := readRecord(t, record)
	if rec.Dir != filepath.Join(abs, "work") {
		t.Errorf("cwd = %s, want %s", rec.Dir, filepath.Join(abs, "work"))
	}
	if !slices.Contains(rec.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(abs, "config")) {
		t.Errorf("config dir is not the absolute state dir; the CLI resolves a relative one against its own cwd: %q", rec.Env)
	}
}

func TestCLIStepForgetsOldestSession(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	c, record := fakeCLIIn(t, "structured", state)
	for i := 0; i <= maxSessions; i++ {
		step(t, c, StepRequest{Prompt: fmt.Sprintf("p%d\n", i), Tools: []string{}})
	}
	recs := readRecords(t, record)
	_, first := sessionFlag(recs[0].Args)
	_, second := sessionFlag(recs[1].Args)
	if exists(fakeTranscript(state, first)) {
		t.Error("transcript of the oldest session was kept past the limit")
	}
	if !exists(fakeTranscript(state, second)) {
		t.Error("transcript of a session within the limit was removed")
	}
	step(t, c, StepRequest{Prompt: "p0\nmore\n", Tools: []string{}})
	recs = readRecords(t, record)
	if flag, _ := sessionFlag(recs[len(recs)-1].Args); flag != "--session-id" {
		t.Errorf("extending a forgotten session used %s, want a new session", flag)
	}
}

func TestCLIStepClearsLeftoverTranscripts(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	old, keep := fakeTranscript(state, "leftover"), filepath.Join(state, "keep.txt")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{old, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := fakeCLIIn(t, "structured", state)
	step(t, c, StepRequest{Prompt: "one\n", Tools: []string{}})
	if exists(old) {
		t.Error("transcript left by an earlier process was kept")
	}
	if !exists(keep) {
		t.Error("a file outside the CLI's transcripts was removed")
	}
}

func TestCLIStepConcurrentExtensionsShareNoSession(t *testing.T) {
	t.Parallel()
	c, record := fakeCLI(t, "structured")
	base := StepRequest{Prompt: "base\n", Tools: []string{}}
	step(t, c, base)
	var wg sync.WaitGroup
	for _, suffix := range []string{"a\n", "b\n"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := base
			r.Prompt += suffix
			if _, err := c.Step(context.Background(), r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	resumes := 0
	for _, rec := range readRecords(t, record)[1:] {
		if flag, _ := sessionFlag(rec.Args); flag == "--resume" {
			resumes++
		}
	}
	if resumes != 1 {
		t.Errorf("%d of 2 concurrent steps resumed the same session, want exactly 1", resumes)
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

func TestCLIStepCountsCachedInput(t *testing.T) {
	c, _ := fakeCLI(t, "cached")
	resp, err := c.Step(context.Background(), StepRequest{Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 2+1458+9000 || resp.Usage.OutputTokens != 53 || resp.Usage.CacheReadTokens != 9000 {
		t.Errorf("usage = %+v, want cache creation and cache reads counted as input, cache reads also kept apart", resp.Usage)
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
	for _, tc := range []struct{ mode, leaked string }{
		{"error", "Invalid API key"},
		{"crash", "boom: native binary missing"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c, _ := fakeCLI(t, tc.mode)
			_, err := c.Step(context.Background(), StepRequest{Tools: []string{}})
			if err == nil || strings.Contains(err.Error(), tc.leaked) {
				t.Errorf("err = %v, want an error without %q", err, tc.leaked)
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

type streamBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

func streamContent(t *testing.T, stdin string) []streamBlock {
	t.Helper()
	if strings.Count(strings.TrimSpace(stdin), "\n") != 0 {
		t.Fatalf("stdin is not one stream-json line: %q", stdin)
	}
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Role    string        `json:"role"`
			Content []streamBlock `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(stdin), &msg); err != nil {
		t.Fatalf("stdin is not JSON: %v: %q", err, stdin)
	}
	if msg.Type != "user" || msg.Message.Role != "user" {
		t.Errorf("message type=%q role=%q, want a user message", msg.Type, msg.Message.Role)
	}
	return msg.Message.Content
}

func wantStreamFlags(t *testing.T, args []string, stream bool) {
	t.Helper()
	i := slices.Index(args, "--output-format")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --output-format in %q", args)
	}
	if stream {
		if args[i+1] != "stream-json" || !slices.Contains(args, "--verbose") || !slices.Contains(args, "--input-format") || args[slices.Index(args, "--input-format")+1] != "stream-json" {
			t.Errorf("args = %q, want stream-json in and out with --verbose", args)
		}
		return
	}
	if args[i+1] != "json" || slices.Contains(args, "--input-format") || slices.Contains(args, "--verbose") {
		t.Errorf("args = %q, want plain --output-format json", args)
	}
}

var (
	pngA = task.Image{MediaType: "image/png", Data: []byte("png-a")}
	jpgB = task.Image{MediaType: "image/jpeg", Data: []byte("jpg-b")}
)

func TestCLIStepSendsImagesAsStreamJSON(t *testing.T) {
	t.Parallel()
	c, record := fakeCLI(t, "stream")
	req := StepRequest{System: "s", Prompt: "tools\n[tool result id=c1] shot\n[image 1 from tool call c1]\n[user] what?\n", Schema: json.RawMessage(`{}`), Tools: []string{}, Images: []task.Image{pngA}}
	resp, err := c.Step(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != `{"text":"red"}` {
		t.Errorf("text = %q", resp.Text)
	}
	rec := readRecord(t, record)
	wantStreamFlags(t, rec.Args, true)
	if flag, id := sessionFlag(rec.Args); flag != "--session-id" || !uuidRe.MatchString(id) {
		t.Errorf("session flag = %s %s", flag, id)
	}
	got := streamContent(t, rec.Stdin)
	if len(got) != 3 || got[0].Type != "text" || got[0].Text != "tools\n[tool result id=c1] shot\n[image 1 from tool call c1]" ||
		got[1].Type != "image" || got[1].Source.Type != "base64" || got[1].Source.MediaType != "image/png" || got[1].Source.Data != base64.StdEncoding.EncodeToString(pngA.Data) ||
		got[2].Type != "text" || got[2].Text != "\n[user] what?\n" {
		t.Errorf("content = %+v", got)
	}
}

func TestCLIStepResumedDeltaCarriesOnlyNewImage(t *testing.T) {
	t.Parallel()
	c, record := fakeCLI(t, "stream")
	req := StepRequest{Prompt: "[tool result id=c1] a\n[image 1 from tool call c1]\n", Tools: []string{}, Images: []task.Image{pngA}}
	step(t, c, req)
	req.Prompt += "[user] thanks\n"
	step(t, c, req)
	req.Prompt += "[tool result id=c2] b\n[image 2 from tool call c2]\n"
	req.Images = append(req.Images, jpgB)
	step(t, c, req)
	recs := readRecords(t, record)
	if len(recs) != 3 {
		t.Fatalf("CLI ran %d times, want 3", len(recs))
	}
	_, id := sessionFlag(recs[0].Args)
	for i, rec := range recs[1:] {
		if flag, got := sessionFlag(rec.Args); flag != "--resume" || got != id {
			t.Errorf("step %d: %s %s, want --resume %s", i+2, flag, got, id)
		}
	}
	wantStreamFlags(t, recs[1].Args, false)
	if recs[1].Stdin != "[user] thanks\n" {
		t.Errorf("text-only delta stdin = %q, want plain text", recs[1].Stdin)
	}
	wantStreamFlags(t, recs[2].Args, true)
	got := streamContent(t, recs[2].Stdin)
	if len(got) != 3 || got[0].Text != "[tool result id=c2] b\n[image 2 from tool call c2]" ||
		got[1].Type != "image" || got[1].Source.MediaType != "image/jpeg" || got[1].Source.Data != base64.StdEncoding.EncodeToString(jpgB.Data) ||
		got[2].Type != "text" || got[2].Text != "\n" {
		t.Errorf("resumed content = %+v, want only image 2", got)
	}
}

func TestParseStreamResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, out string
		ok        bool
		want      string
	}{
		{"takes the result line", "{\"type\":\"system\",\"subtype\":\"init\"}\n{\"type\":\"assistant\",\"message\":{}}\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"x\",\"structured_output\":{\"text\":\"red\"},\"usage\":{\"input_tokens\":4,\"cache_read_input_tokens\":3767,\"output_tokens\":2}}\n", true, `{"text":"red"}`},
		{"no result line", "{\"type\":\"system\",\"subtype\":\"init\"}\n", false, ""},
		{"empty", "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := parseStreamResult([]byte(tc.out))
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if tc.ok && (string(res.StructuredOutput) != tc.want || res.Usage.CacheRead != 3767 || res.Usage.InputTokens != 4 || res.Subtype != "success") {
				t.Errorf("res = %+v", res)
			}
		})
	}
}
