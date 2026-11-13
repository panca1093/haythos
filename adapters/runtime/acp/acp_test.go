package acp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/panca/haythos/adapters/runtime/acp"
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

func helperConfig(t *testing.T, s runtimecontract.Script, extra ...string) acp.Config {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return acp.Config{
		Name: "helper", Command: os.Args[0], Args: []string{"-test.run=^$"},
		Env: append([]string{helperEnv + "=1", "HAYTHOS_ACP_SCRIPT=" + string(raw)}, extra...), StopGrace: 3 * time.Second,
	}
}

func TestRuntimeContract(t *testing.T) {
	runtimecontract.RunRuntimeContract(t, func(t *testing.T, s runtimecontract.Script) app.Runtime {
		return acp.New(helperConfig(t, s))
	})
}

func startSession(t *testing.T, cfg acp.Config, req app.RuntimeStart) (*acp.Runtime, app.RuntimeSession) {
	t.Helper()
	rt := acp.New(cfg)
	if req.Workdir == "" {
		req.Workdir = t.TempDir()
	}
	sess, err := rt.Start(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })
	return rt, sess
}

func readLog(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestChildEnvHoldsOnlyTheNamedVariables(t *testing.T) {
	t.Setenv("SECRET_API_KEY", "s3cret")
	t.Setenv("PASSED_VAR", "yes")
	logPath := filepath.Join(t.TempDir(), "log")
	cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_ACP_LOG="+logPath, "OPENCODE_CONFIG_CONTENT={}")
	cfg.PassEnv = []string{"PASSED_VAR", "NOT_SET_ANYWHERE"}
	startSession(t, cfg, app.RuntimeStart{TaskID: "task-1", SessionID: "sess-1"})

	var names []string
	for _, l := range readLog(t, logPath) {
		if rest, ok := strings.CutPrefix(l, "env "); ok {
			names = append(names, strings.Fields(rest)...)
		}
	}
	got := map[string]string{}
	for _, kv := range names {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	if got["HAYTHOS_TASK"] != "task-1" || got["HAYTHOS_SESSION"] != "sess-1" || got["PASSED_VAR"] != "yes" || got["OPENCODE_CONFIG_CONTENT"] != "{}" {
		t.Errorf("env = %v, want task, session, passed and extra variables", got)
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
}

func TestHandshakeAndConfigOptions(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_ACP_LOG="+logPath)
	cfg.ConfigOptions = map[string]string{"model": "mdl"}
	rt, sess := startSession(t, cfg, app.RuntimeStart{Model: "zen/free", Mode: "plan"})
	if sess.Ref() != "ses_helper" {
		t.Errorf("Ref = %q", sess.Ref())
	}
	want := app.RuntimeCaps{Resume: true, Images: true, Modes: []string{"build", "plan"}}
	if diff := cmp.Diff(want, rt.Capabilities()); diff != "" {
		t.Errorf("capabilities mismatch (-want +got):\n%s", diff)
	}

	var methods []string
	var init, setOpts []map[string]any
	for _, l := range readLog(t, logPath) {
		raw, ok := strings.CutPrefix(l, "frame ")
		if !ok {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(raw), &f); err != nil {
			t.Fatal(err)
		}
		m, _ := f["method"].(string)
		methods = append(methods, m)
		p, _ := f["params"].(map[string]any)
		switch m {
		case "initialize":
			init = append(init, p)
		case "session/set_config_option":
			setOpts = append(setOpts, p)
		}
	}
	if diff := cmp.Diff([]string{"initialize", "session/new", "session/set_config_option", "session/set_config_option"}, methods); diff != "" {
		t.Errorf("methods mismatch (-want +got):\n%s", diff)
	}
	caps, _ := init[0]["clientCapabilities"].(map[string]any)
	fsCaps, _ := caps["fs"].(map[string]any)
	if init[0]["protocolVersion"] != float64(1) || caps["terminal"] != false || fsCaps["readTextFile"] != false || fsCaps["writeTextFile"] != false {
		t.Errorf("initialize params = %v, want protocol 1 and no fs or terminal", init[0])
	}
	var pairs []string
	for _, p := range setOpts {
		pairs = append(pairs, p["configId"].(string)+"="+p["value"].(string))
	}
	slices.Sort(pairs)
	if diff := cmp.Diff([]string{"mdl=zen/free", "mode=plan"}, pairs); diff != "" {
		t.Errorf("set_config_option mismatch (-want +got):\n%s", diff)
	}
}

func TestResumeLoadsTheSessionAndDropsTheReplay(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	cfg := helperConfig(t, runtimecontract.Script{}, "HAYTHOS_ACP_LOG="+logPath)
	_, sess := startSession(t, cfg, app.RuntimeStart{ResumeRef: "ses_old"})
	if sess.Ref() != "ses_old" {
		t.Errorf("Ref = %q, want ses_old", sess.Ref())
	}
	if _, err := sess.Send(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	for ev := range drain(t, sess) {
		if ev.Kind == app.RuntimeMessage {
			t.Errorf("replayed history reached Events: %+v", ev)
		}
	}
	if !strings.Contains(strings.Join(readLog(t, logPath), "\n"), `"method":"session/load"`) {
		t.Error("session/load was not called")
	}
}

func TestStartFailures(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(t *testing.T) acp.Config
		req  app.RuntimeStart
		want string
	}{
		{"missing command", func(*testing.T) acp.Config { return acp.Config{Name: "x", Command: "/nonexistent/acp-agent"} }, app.RuntimeStart{}, "start x"},
		{"agent exits before answering", func(*testing.T) acp.Config {
			return acp.Config{Name: "x", Command: "/usr/bin/false"}
		}, app.RuntimeStart{}, "initialize"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Workdir = t.TempDir()
			_, err := acp.New(tc.cfg(t)).Start(context.Background(), tc.req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// drain stops the session and returns what was left on Events.
func drain(t *testing.T, sess app.RuntimeSession) <-chan app.RuntimeEvent {
	t.Helper()
	if err := sess.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := make(chan app.RuntimeEvent, 1024)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				close(out)
				return out
			}
			out <- ev
		case <-time.After(5 * time.Second):
			t.Fatal("Events not closed after Stop")
		}
	}
}

func TestEventMappingAndPermissionRequest(t *testing.T) {
	script := runtimecontract.Script{Steps: []runtimecontract.Step{runtimecontract.Say("he", "llo"), runtimecontract.Ask("execute", "cd work && rm victim.txt")}}
	_, sess := startSession(t, helperConfig(t, script), app.RuntimeStart{})
	type result struct {
		stop app.StopReason
		err  error
	}
	done := make(chan result, 1)
	go func() {
		stop, err := sess.Send(context.Background(), "go")
		done <- result{stop, err}
	}()
	var got []app.RuntimeEvent
	for ev := range sess.Events() {
		got = append(got, ev)
		if ev.Kind == app.RuntimePermissionRequest {
			if err := sess.Approve(context.Background(), ev.Permission.RequestID, ev.Permission.RejectOptionID); err != nil {
				t.Fatal(err)
			}
			if err := sess.Approve(context.Background(), ev.Permission.RequestID, ev.Permission.RejectOptionID); err == nil {
				t.Error("a second answer to the same request was accepted")
			}
		}
		if ev.Kind == app.RuntimeStop {
			break
		}
	}
	res := <-done
	if res.err != nil || res.stop != app.StopEndTurn {
		t.Fatalf("Send = %v, %v, want end_turn", res.stop, res.err)
	}
	want := []app.RuntimeEvent{
		{Kind: app.RuntimeMessage, ID: "m1", Text: "he"},
		{Kind: app.RuntimeMessage, ID: "m1", Text: "llo"},
		{Kind: app.RuntimeToolCall, ID: "tc1", Text: "cd work && rm victim.txt", ToolKind: "execute", Status: "pending"},
		{Kind: app.RuntimePermissionRequest, ID: "tc1", Text: "cd work && rm victim.txt", ToolKind: "execute", Permission: &app.PermissionRequest{
			RequestID: "101", Tool: "shell", Kind: "execute", Text: "cd work && rm victim.txt", AllowOptionID: "once", RejectOptionID: "reject",
		}},
		{Kind: app.RuntimeStop, Stop: app.StopEndTurn},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("events mismatch (-want +got):\n%s", diff)
	}
}

func TestStopKillsTheProcessAndIsIdempotent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "log")
	cfg := helperConfig(t, runtimecontract.Script{Steps: []runtimecontract.Step{runtimecontract.Hang()}}, "HAYTHOS_ACP_LOG="+logPath)
	_, sess := startSession(t, cfg, app.RuntimeStart{})
	var pid int
	for _, l := range readLog(t, logPath) {
		if rest, ok := strings.CutPrefix(l, "pid "); ok {
			pid = atoi(t, rest)
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
	if _, ok := <-sess.Events(); ok {
		for range sess.Events() {
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive after Stop", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &n); err != nil {
		t.Fatal(err)
	}
	return n
}
