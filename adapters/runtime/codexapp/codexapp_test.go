package codexapp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/panca/haythos/adapters/runtime/codexapp"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/tests/runtimecontract"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		runHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// helperConfig points the runtime at the test binary acting as the app-server; codexHome is a scratch owner dir with
// a fake auth.json.
func helperConfig(t *testing.T, s runtimecontract.Script, extra ...string) codexapp.Config {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	owner := t.TempDir()
	if err := os.WriteFile(filepath.Join(owner, "auth.json"), []byte(`{"fake":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return codexapp.Config{
		Command: os.Args[0], Args: []string{"-test.run=^$"}, CodexHome: owner,
		Env: append([]string{helperEnv + "=1", "HAYTHOS_CODEX_SCRIPT=" + string(raw)}, extra...), StopGrace: 3 * time.Second,
	}
}

func TestRuntimeContract(t *testing.T) {
	runtimecontract.RunRuntimeContract(t, func(t *testing.T, s runtimecontract.Script) app.Runtime {
		return codexapp.New(helperConfig(t, s))
	})
}

func startSession(t *testing.T, cfg codexapp.Config, req app.RuntimeStart) app.RuntimeSession {
	t.Helper()
	if req.Workdir == "" {
		req.Workdir = t.TempDir()
	}
	sess, err := codexapp.New(cfg).Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })
	return sess
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func envOf(t *testing.T, lines []string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, "env "); ok {
			for _, kv := range strings.Fields(rest) {
				k, v, _ := strings.Cut(kv, "=")
				got[k] = v
			}
		}
	}
	return got
}

func TestChildEnvAndIsolatedCodexHome(t *testing.T) {
	t.Setenv("SECRET_API_KEY", "s3cret")
	t.Setenv("PASSED_VAR", "yes")
	t.Setenv("CODEX_HOME", "/owner/real/.codex")
	logPath := filepath.Join(t.TempDir(), "log")
	cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_CODEX_LOG="+logPath)
	cfg.PassEnv = []string{"PASSED_VAR", "NOT_SET_ANYWHERE"}
	sess := startSession(t, cfg, app.RuntimeStart{TaskID: "task-1", SessionID: "sess-1"})

	got := envOf(t, readLog(t, logPath))
	if got["HAYTHOS_TASK"] != "task-1" || got["HAYTHOS_SESSION"] != "sess-1" || got["PASSED_VAR"] != "yes" {
		t.Errorf("env = %v, want task, session and passed variable", got)
	}
	for k := range got {
		if strings.Contains(k, "SECRET") || k == "NOT_SET_ANYWHERE" {
			t.Errorf("child env leaks %s", k)
		}
	}
	for _, k := range []string{"PATH", "HOME", "USER"} {
		if _, ok := got[k]; !ok {
			t.Errorf("child env lacks %s", k)
		}
	}
	home := got["CODEX_HOME"]
	if home == "" || strings.Contains(home, "/owner/real") || strings.HasPrefix(home, cfg.CodexHome) {
		t.Fatalf("CODEX_HOME = %q, want a fresh directory", home)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Errorf("session home holds %v, want only auth.json", entries)
	}
	if dest, err := os.Readlink(filepath.Join(home, "auth.json")); err != nil || dest != filepath.Join(cfg.CodexHome, "auth.json") {
		t.Errorf("auth.json -> %q, %v, want the owner's file", dest, err)
	}
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("session home still exists after Stop: %v", err)
	}
}

func TestHandshake(t *testing.T) {
	tests := []struct {
		name      string
		cfgModel  string
		reqModel  string
		wantModel any
	}{
		{"no model", "", "", nil},
		{"config model", "gpt-config", "", "gpt-config"},
		{"request wins", "gpt-config", "gpt-req", "gpt-req"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "log")
			cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_CODEX_LOG="+logPath)
			cfg.Model = tc.cfgModel
			workdir := t.TempDir()
			sess := startSession(t, cfg, app.RuntimeStart{Workdir: workdir, Model: tc.reqModel})
			if sess.Ref() != "thr_helper" {
				t.Errorf("Ref = %q, want the thread id", sess.Ref())
			}
			var methods []string
			var threadParams map[string]any
			for _, l := range readLog(t, logPath) {
				raw, ok := strings.CutPrefix(l, "frame ")
				if !ok {
					continue
				}
				var f map[string]any
				if err := json.Unmarshal([]byte(raw), &f); err != nil {
					t.Fatal(err)
				}
				if _, has := f["jsonrpc"]; has {
					t.Errorf("frame carries jsonrpc: %s", raw)
				}
				m, _ := f["method"].(string)
				methods = append(methods, m)
				if m == "thread/start" {
					threadParams, _ = f["params"].(map[string]any)
				}
			}
			if diff := cmp.Diff([]string{"initialize", "initialized", "thread/start"}, methods); diff != "" {
				t.Errorf("methods mismatch (-want +got):\n%s", diff)
			}
			want := map[string]any{"cwd": workdir, "approvalPolicy": "untrusted", "sandbox": "danger-full-access", "ephemeral": true}
			if tc.wantModel != nil {
				want["model"] = tc.wantModel
			}
			if diff := cmp.Diff(want, threadParams); diff != "" {
				t.Errorf("thread/start params mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestStartFailures(t *testing.T) {
	tests := []struct {
		name string
		cfg  codexapp.Config
		req  app.RuntimeStart
		want string
	}{
		{"missing command", codexapp.Config{Command: "/nonexistent/codex"}, app.RuntimeStart{}, "start codex"},
		{"exits before answering", codexapp.Config{Command: "/usr/bin/false"}, app.RuntimeStart{}, "initialize"},
		{"resume unsupported", codexapp.Config{Command: "/usr/bin/false"}, app.RuntimeStart{ResumeRef: "thr_old"}, "cannot be resumed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Workdir = t.TempDir()
			tc.cfg.CodexHome = t.TempDir()
			_, err := codexapp.New(tc.cfg).Start(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// runTurn plays one turn against the helper and returns every event up to and including stop, and Send's result.
func runTurn(t *testing.T, sess app.RuntimeSession, answer string) (events []app.RuntimeEvent, stop app.StopReason, err error) {
	t.Helper()
	type result struct {
		stop app.StopReason
		err  error
	}
	done := make(chan result, 1)
	go func() {
		s, err := sess.Send(context.Background(), "go")
		done <- result{s, err}
	}()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("events closed mid-turn")
			}
			events = append(events, ev)
			if ev.Kind == app.RuntimePermissionRequest {
				if err := sess.Approve(context.Background(), ev.Permission.RequestID, answer); err != nil {
					t.Fatal(err)
				}
			}
		case r := <-done:
			for {
				select {
				case ev := <-sess.Events():
					events = append(events, ev)
				default:
					return events, r.stop, r.err
				}
			}
		case <-timeout:
			t.Fatal("turn did not finish")
		}
	}
}

func TestEventMapping(t *testing.T) {
	item := func(typ string, extra map[string]any) map[string]any {
		it := map[string]any{"id": "i1", "type": typ}
		for k, v := range extra {
			it[k] = v
		}
		return it
	}
	note := func(method string, params map[string]any) map[string]any {
		params["threadId"] = "thr_helper"
		return map[string]any{"method": method, "params": params}
	}
	tests := []struct {
		name   string
		frames []map[string]any
		want   []app.RuntimeEvent
	}{
		{"agent message deltas", []map[string]any{
			note("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": "he"}),
			note("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": "llo"}),
			note("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": ""}),
		}, []app.RuntimeEvent{{Kind: app.RuntimeMessage, ID: "m1", Text: "he"}, {Kind: app.RuntimeMessage, ID: "m1", Text: "llo"}}},
		{"reasoning deltas", []map[string]any{
			note("item/reasoning/textDelta", map[string]any{"itemId": "r1", "delta": "think"}),
			note("item/reasoning/summaryTextDelta", map[string]any{"itemId": "r1", "delta": "sum"}),
		}, []app.RuntimeEvent{{Kind: app.RuntimeThought, ID: "r1", Text: "think"}, {Kind: app.RuntimeThought, ID: "r1", Text: "sum"}}},
		{"command item", []map[string]any{
			note("item/started", map[string]any{"item": item("commandExecution", map[string]any{"command": "go vet ./...", "status": "inProgress"})}),
			note("item/completed", map[string]any{"item": item("commandExecution", map[string]any{"command": "go vet ./...", "status": "completed"})}),
		}, []app.RuntimeEvent{
			{Kind: app.RuntimeToolCall, ID: "i1", Text: "go vet ./...", ToolKind: "execute", Status: "in_progress"},
			{Kind: app.RuntimeToolCallUpdate, ID: "i1", Text: "go vet ./...", ToolKind: "execute", Status: "completed"},
		}},
		{"declined file change", []map[string]any{
			note("item/started", map[string]any{"item": item("fileChange", map[string]any{"status": "inProgress", "changes": []any{map[string]any{"path": "a.go"}, map[string]any{"path": "b.go"}}})}),
			note("item/completed", map[string]any{"item": item("fileChange", map[string]any{"status": "declined", "changes": []any{map[string]any{"path": "a.go"}, map[string]any{"path": "b.go"}}})}),
		}, []app.RuntimeEvent{
			{Kind: app.RuntimeToolCall, ID: "i1", Text: "a.go, b.go", ToolKind: "edit", Status: "in_progress"},
			{Kind: app.RuntimeToolCallUpdate, ID: "i1", Text: "a.go, b.go", ToolKind: "edit", Status: "failed"},
		}},
		{"mcp and web search", []map[string]any{
			note("item/started", map[string]any{"item": item("mcpToolCall", map[string]any{"server": "gh", "tool": "pr", "status": "inProgress"})}),
			note("item/started", map[string]any{"item": item("webSearch", map[string]any{"query": "golang"})}),
		}, []app.RuntimeEvent{
			{Kind: app.RuntimeToolCall, ID: "i1", Text: "gh.pr", ToolKind: "other", Status: "in_progress"},
			{Kind: app.RuntimeToolCall, ID: "i1", Text: "golang", ToolKind: "fetch"},
		}},
		{"plan", []map[string]any{
			note("turn/plan/updated", map[string]any{"plan": []any{map[string]any{"step": "one", "status": "pending"}, map[string]any{"step": "two", "status": "pending"}}}),
		}, []app.RuntimeEvent{{Kind: app.RuntimePlan, Text: "one\ntwo"}}},
		{"ignored items and other threads", []map[string]any{
			note("item/started", map[string]any{"item": item("agentMessage", nil)}),
			note("item/started", map[string]any{"item": item("reasoning", nil)}),
			{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thr_other", "itemId": "x", "delta": "sub-agent"}},
			note("mcpServer/startupStatus/updated", map[string]any{"name": "x"}),
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.frames)
			if err != nil {
				t.Fatal(err)
			}
			cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_CODEX_RAW="+string(raw), "HAYTHOS_CODEX_STATUS=completed")
			events, stop, err := runTurn(t, startSession(t, cfg, app.RuntimeStart{}), "accept")
			if err != nil || stop != app.StopEndTurn {
				t.Fatalf("Send = %v, %v, want end_turn", stop, err)
			}
			want := append(tc.want, app.RuntimeEvent{Kind: app.RuntimeStop, Stop: app.StopEndTurn})
			if diff := cmp.Diff(want, events); diff != "" {
				t.Errorf("events mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTurnStatusToStopReason(t *testing.T) {
	tests := []struct {
		status  string
		want    app.StopReason
		wantErr string
	}{
		{"completed", app.StopEndTurn, ""},
		{"interrupted", app.StopCancelled, ""},
		{"failed", "", "boom"},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_CODEX_RAW=[]", "HAYTHOS_CODEX_STATUS="+tc.status)
			events, stop, err := runTurn(t, startSession(t, cfg, app.RuntimeStart{}), "accept")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if len(events) != 0 {
					t.Errorf("a failed turn emitted %v, want no stop event", events)
				}
				return
			}
			if err != nil || stop != tc.want {
				t.Fatalf("Send = %v, %v, want %s", stop, err, tc.want)
			}
		})
	}
}

func TestApprovalRequests(t *testing.T) {
	tests := []struct {
		name    string
		step    runtimecontract.Step
		want    app.PermissionRequest
		answer  string
		decided string
	}{
		{"command accepted", runtimecontract.Ask("execute", "go test ./..."), app.PermissionRequest{
			RequestID: "0", Tool: "commandExecution", Kind: "execute", Text: "go test ./...", AllowOptionID: "accept", RejectOptionID: "decline",
		}, "accept", "accept"},
		{"command declined", runtimecontract.Ask("execute", "rm victim.txt"), app.PermissionRequest{
			RequestID: "0", Tool: "commandExecution", Kind: "execute", Text: "rm victim.txt", AllowOptionID: "accept", RejectOptionID: "decline",
		}, "decline", "decline"},
		{"file change takes its paths from the item", runtimecontract.Ask("edit", "main.go"), app.PermissionRequest{
			RequestID: "0", Tool: "fileChange", Kind: "edit", Text: "main.go", AllowOptionID: "accept", RejectOptionID: "decline",
		}, "accept", "accept"},
		{"empty option cancels", runtimecontract.Ask("execute", "sleep 1"), app.PermissionRequest{
			RequestID: "0", Tool: "commandExecution", Kind: "execute", Text: "sleep 1", AllowOptionID: "accept", RejectOptionID: "decline",
		}, "", "cancel"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := helperConfig(t, runtimecontract.Script{Steps: []runtimecontract.Step{tc.step, runtimecontract.Write("out.txt", "x")}})
			workdir := t.TempDir()
			events, stop, err := runTurn(t, startSession(t, cfg, app.RuntimeStart{Workdir: workdir}), tc.answer)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			var got *app.PermissionRequest
			for _, ev := range events {
				if ev.Kind == app.RuntimePermissionRequest {
					got = ev.Permission
					if ev.Text != tc.want.Text || ev.ToolKind != tc.want.Kind {
						t.Errorf("event text/kind = %q/%q, want %q/%q", ev.Text, ev.ToolKind, tc.want.Text, tc.want.Kind)
					}
				}
			}
			if diff := cmp.Diff(&tc.want, got); diff != "" {
				t.Errorf("permission mismatch (-want +got):\n%s", diff)
			}
			_, werr := os.Stat(filepath.Join(workdir, "out.txt"))
			switch tc.decided {
			case "accept":
				if werr != nil || stop != app.StopEndTurn {
					t.Errorf("after accept: stat %v, stop %s, want the write and end_turn", werr, stop)
				}
			case "decline":
				if werr == nil {
					t.Error("the write ran after decline")
				}
			case "cancel":
				if werr == nil || stop != app.StopCancelled {
					t.Errorf("after cancel: stat %v, stop %s, want no write and cancelled", werr, stop)
				}
			}
		})
	}
}

func TestApproveRejectsBadAnswers(t *testing.T) {
	cfg := helperConfig(t, runtimecontract.Script{Steps: []runtimecontract.Step{runtimecontract.Ask("execute", "ls"), runtimecontract.Hang()}})
	sess := startSession(t, cfg, app.RuntimeStart{})
	go func() { _, _ = sess.Send(context.Background(), "go") }()
	var req *app.PermissionRequest
	for ev := range sess.Events() {
		if ev.Kind == app.RuntimePermissionRequest {
			req = ev.Permission
			break
		}
	}
	if err := sess.Approve(context.Background(), req.RequestID, "acceptForSession"); err == nil {
		t.Error("an option the adapter does not offer was accepted")
	}
	if err := sess.Approve(context.Background(), "99", "accept"); err == nil {
		t.Error("an answer to an unknown request was accepted")
	}
	if err := sess.Approve(context.Background(), req.RequestID, "decline"); err != nil {
		t.Fatal(err)
	}
	if err := sess.Approve(context.Background(), req.RequestID, "decline"); err == nil {
		t.Error("a second answer to the same request was accepted")
	}
}

func TestStopKillsTheProcessAndIsIdempotent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	cfg := helperConfig(t, runtimecontract.Script{Steps: []runtimecontract.Step{runtimecontract.Hang()}}, "HAYTHOS_CODEX_LOG="+logPath)
	sess := startSession(t, cfg, app.RuntimeStart{})
	var pid int
	for _, l := range readLog(t, logPath) {
		if rest, ok := strings.CutPrefix(l, "pid "); ok {
			pid, _ = strconv.Atoi(strings.TrimSpace(rest))
		}
	}
	type result struct {
		stop app.StopReason
		err  error
	}
	done := make(chan result, 1)
	go func() {
		stop, err := sess.Send(context.Background(), "hang")
		done <- result{stop, err}
	}()
	time.Sleep(100 * time.Millisecond)
	for range 2 {
		if err := sess.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case r := <-done:
		if r.err != nil || r.stop != app.StopCancelled {
			t.Errorf("Send = %v, %v, want cancelled", r.stop, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send did not return after Stop")
	}
	for range sess.Events() {
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive after Stop", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
