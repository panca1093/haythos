package claudecli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/tests/runtimecontract"
)

const (
	scriptEnv = "CLAUDECLI_FAKE_SCRIPT"
	recordEnv = "CLAUDECLI_FAKE_RECORD"
)

type fakeRecord struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Dir  string   `json:"dir"`
}

type obj = map[string]any

// TestFakeClaude is a scripted claude CLI: the test binary re-execs itself into it. It plays a runtimecontract.Script on the first user message.
func TestFakeClaude(t *testing.T) {
	raw := os.Getenv(scriptEnv)
	if raw == "" {
		return
	}
	var script runtimecontract.Script
	if err := json.Unmarshal([]byte(raw), &script); err != nil {
		os.Exit(2)
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	dir, _ := os.Getwd()
	if p := os.Getenv(recordEnv); p != "" {
		b, _ := json.Marshal(fakeRecord{Args: args, Env: os.Environ(), Dir: dir})
		_ = os.WriteFile(p, b, 0o600)
	}
	in := make(chan obj, 16)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			var f obj
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				in <- f
			}
		}
		close(in)
	}()
	enc := json.NewEncoder(os.Stdout)
	send := func(f obj) { _ = enc.Encode(f) }
	reply := func(f obj) {
		send(obj{"type": "control_response", "response": obj{"subtype": "success", "request_id": f["request_id"], "response": obj{}}})
	}
	result := func(subtype, terminal string, isErr bool) {
		send(obj{"type": "result", "subtype": subtype, "is_error": isErr, "stop_reason": "end_turn", "terminal_reason": terminal})
	}
	subtype := func(f obj) string {
		r, _ := f["request"].(map[string]any)
		s, _ := r["subtype"].(string)
		return s
	}
	// next returns the next obj of type typ, answering initialize and interrupt on the way; interrupted is true when the turn was interrupted.
	next := func(typ string) (f obj, interrupted bool) {
		for f := range in {
			if f["type"] == "control_request" {
				switch subtype(f) {
				case "interrupt":
					reply(f)
					result("error_during_execution", "aborted_streaming", true)
					return nil, true
				case "initialize":
					reply(f)
				}
				continue
			}
			if f["type"] == typ {
				return f, false
			}
		}
		os.Exit(0)
		return nil, false
	}
	if _, intr := next("user"); intr {
		return
	}
	send(obj{"type": "system", "subtype": "init", "session_id": "ref-from-init"})
	allowed := true
	for i, st := range script.Steps {
		switch st.Kind {
		case runtimecontract.StepSay:
			for _, c := range st.Chunks {
				send(obj{"type": "assistant", "message": obj{"id": "msg_1", "content": []any{obj{"type": "text", "text": c}}}})
			}
		case runtimecontract.StepAsk:
			id := "toolu_" + string(rune('a'+i))
			tool := map[string]obj{
				"execute": {"command": st.Text}, "read": {"file_path": st.Text}, "edit": {"file_path": st.Text},
			}[st.ReqKind]
			name := map[string]string{"execute": "Bash", "read": "Read", "edit": "Edit"}[st.ReqKind]
			send(obj{"type": "assistant", "message": obj{"id": "msg_2", "content": []any{obj{"type": "tool_use", "id": id, "name": name, "input": tool}}}})
			send(obj{"type": "control_request", "request_id": "req-" + id, "request": obj{"subtype": "can_use_tool", "tool_name": name, "input": tool, "tool_use_id": id}})
			var answer obj
			for f := range in {
				if f["type"] == "control_response" {
					answer, _ = f["response"].(map[string]any)
					break
				}
				if f["type"] == "control_request" && subtype(f) == "interrupt" {
					reply(f)
					result("error_during_execution", "aborted_streaming", true)
					return
				}
			}
			resp, _ := answer["response"].(map[string]any)
			ok := resp["behavior"] == "allow" && resp["updatedInput"] != nil && answer["request_id"] == "req-"+id
			allowed = allowed && ok
			send(obj{"type": "user", "message": obj{"content": []any{obj{"type": "tool_result", "tool_use_id": id, "is_error": !ok}}}})
			if !ok && script.RejectEndsTurn {
				result("error_during_execution", "aborted_tools", true)
				return
			}
		case runtimecontract.StepWrite:
			if allowed {
				_ = os.WriteFile(filepath.Join(dir, st.Path), []byte(st.Content), 0o600)
			}
		case runtimecontract.StepCrash:
			os.Exit(1)
		case runtimecontract.StepHang:
			if _, intr := next("never"); intr {
				return
			}
		}
	}
	result("success", "completed", false)
	for range in {
	}
}

func fakeRuntime(t *testing.T, s runtimecontract.Script) app.Runtime {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(scriptEnv, string(b))
	return New(Config{Command: os.Args[0], Args: []string{"-test.run=^TestFakeClaude$", "--"}, PassEnv: []string{scriptEnv, recordEnv}})
}

func TestRuntimeContract(t *testing.T) {
	runtimecontract.RunRuntimeContract(t, fakeRuntime)
}

func startFake(t *testing.T, cfg Config, req app.RuntimeStart, s runtimecontract.Script) (app.RuntimeSession, fakeRecord) {
	t.Helper()
	b, _ := json.Marshal(s)
	t.Setenv(scriptEnv, string(b))
	rec := filepath.Join(t.TempDir(), "record.json")
	t.Setenv(recordEnv, rec)
	cfg.Command, cfg.Args = os.Args[0], []string{"-test.run=^TestFakeClaude$", "--"}
	cfg.PassEnv = append(cfg.PassEnv, scriptEnv, recordEnv)
	if req.Workdir == "" {
		req.Workdir = t.TempDir()
	}
	sess, err := New(cfg).Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })
	if _, err := sess.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	var r fakeRecord
	raw, err := os.ReadFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return sess, r
}

func TestStartArgsAndEnv(t *testing.T) {
	t.Setenv("LEAKED_SECRET", "x")
	t.Setenv("PASSED", "yes")
	tests := []struct {
		name     string
		cfg      Config
		req      app.RuntimeStart
		wantArgs []string
		noArgs   []string
	}{
		{"new session", Config{Model: "haiku"}, app.RuntimeStart{TaskID: "t1", SessionID: "s1"}, []string{"--permission-prompt-tool", "stdio", "--setting-sources", "project,local", "--model", "haiku", "--session-id"}, []string{"--resume"}},
		{"resume and model override", Config{Model: "haiku"}, app.RuntimeStart{TaskID: "t1", SessionID: "s1", Model: "sonnet", ResumeRef: "abc"}, []string{"--resume", "abc", "--model", "sonnet"}, []string{"--session-id", "haiku"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.PassEnv = []string{"PASSED"}
			_, rec := startFake(t, cfg, tc.req, runtimecontract.Script{})
			joined := strings.Join(rec.Args, " ")
			for _, a := range tc.wantArgs {
				if !strings.Contains(joined, a) {
					t.Errorf("args %q lack %q", joined, a)
				}
			}
			for _, a := range tc.noArgs {
				if slices.Contains(rec.Args, a) {
					t.Errorf("args %q contain %q", joined, a)
				}
			}
			var names []string
			for _, e := range rec.Env {
				names = append(names, e[:strings.IndexByte(e, '=')])
			}
			for _, want := range []string{"PATH", "HOME", "USER", "HAYTHOS_TASK", "HAYTHOS_SESSION", "PASSED"} {
				if !slices.Contains(names, want) {
					t.Errorf("env lacks %s", want)
				}
			}
			if slices.Contains(names, "LEAKED_SECRET") {
				t.Error("env leaked LEAKED_SECRET")
			}
		})
	}
}

func TestSettingsNeverAllowBash(t *testing.T) {
	s, err := New(Config{}).settings()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Permissions struct {
			DefaultMode string   `json:"defaultMode"`
			Allow       []string `json:"allow"`
			Ask         []string `json:"ask"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatal(err)
	}
	if got.Permissions.DefaultMode != "default" {
		t.Errorf("defaultMode = %q, want default", got.Permissions.DefaultMode)
	}
	for _, a := range got.Permissions.Allow {
		if strings.HasPrefix(a, "Bash") {
			t.Errorf("allow rule %q lets a shell command skip the guard", a)
		}
	}
	for _, tool := range []string{"Read", "Grep", "Glob"} {
		for _, p := range []string{"**/.env*", "~/.ssh/**", "~/.aws/**", "~/.netrc", "~/.git-credentials", "~/.gnupg/**"} {
			if rule := tool + "(" + p + ")"; !slices.Contains(got.Permissions.Ask, rule) {
				t.Errorf("ask rules lack %s", rule)
			}
		}
	}
	if _, err := New(Config{Allow: []string{"Read", "Bash"}}).Start(context.Background(), app.RuntimeStart{Workdir: t.TempDir()}); err == nil {
		t.Error("Start accepted a Bash allow rule")
	}
	if _, err := New(Config{}).Start(context.Background(), app.RuntimeStart{Workdir: t.TempDir(), Mode: "bypassPermissions"}); err == nil {
		t.Error("Start accepted mode bypassPermissions")
	}
}

func TestToolKind(t *testing.T) {
	tests := []struct {
		name, tool, input, wantKind, wantText string
	}{
		{"bash", "Bash", `{"command":"rm x"}`, "execute", "rm x"},
		{"read", "Read", `{"file_path":"/w/.env"}`, "read", "/w/.env"},
		{"edit", "Edit", `{"file_path":"/w/a.go"}`, "edit", "/w/a.go"},
		{"fetch", "WebFetch", `{"url":"https://x.test"}`, "fetch", "https://x.test"},
		{"unknown", "mcp__x__y", `{}`, "other", "mcp__x__y"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			kind, text := toolKind(tc.tool, json.RawMessage(tc.input))
			if kind != tc.wantKind || text != tc.wantText {
				t.Errorf("toolKind = (%q, %q), want (%q, %q)", kind, text, tc.wantKind, tc.wantText)
			}
		})
	}
}

func TestStopReason(t *testing.T) {
	tests := []struct {
		name    string
		f       frame
		want    app.StopReason
		wantErr bool
	}{
		{"end turn", frame{Subtype: "success", StopReason: "end_turn", TerminalReason: "completed"}, app.StopEndTurn, false},
		{"max tokens", frame{Subtype: "success", StopReason: "max_tokens"}, app.StopMaxTokens, false},
		{"refusal", frame{Subtype: "success", StopReason: "refusal"}, app.StopRefusal, false},
		{"interrupted", frame{Subtype: "error_during_execution", IsError: true, TerminalReason: "aborted_streaming"}, app.StopCancelled, false},
		{"max turns", frame{Subtype: "error_max_turns", IsError: true}, app.StopMaxTurnRequests, false},
		{"other error", frame{Subtype: "error_during_execution", IsError: true, TerminalReason: "model_error"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stopReason(tc.f)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Errorf("stopReason = (%q, %v), want (%q, err %t)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestStopIsIdempotentAndClosesEvents(t *testing.T) {
	sess, _ := startFake(t, Config{}, app.RuntimeStart{TaskID: "t", SessionID: "s"}, runtimecontract.Script{})
	for i := 0; i < 2; i++ {
		if err := sess.Stop(context.Background()); err != nil {
			t.Fatalf("Stop #%d: %v", i+1, err)
		}
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sess.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Events not closed after Stop")
		}
	}
}
