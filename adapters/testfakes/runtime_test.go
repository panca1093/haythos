package testfakes_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
)

func ask(id string) testfakes.RuntimeStep {
	return testfakes.Emit(app.RuntimeEvent{Kind: app.RuntimePermissionRequest, Permission: &app.PermissionRequest{
		RequestID: id, Kind: "execute", Text: "rm x", AllowOptionID: "allow", RejectOptionID: "reject",
	}})
}

func start(t *testing.T, rt *testfakes.Runtime, dir string) app.RuntimeSession {
	t.Helper()
	s, err := rt.Start(context.Background(), app.RuntimeStart{TaskID: "t", SessionID: "s", Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func drain(s app.RuntimeSession) []app.RuntimeEvent {
	var out []app.RuntimeEvent
	for ev := range s.Events() {
		out = append(out, ev)
	}
	return out
}

func TestRuntimeScriptedTurn(t *testing.T) {
	rt := testfakes.NewRuntime("fake", testfakes.RuntimeScript{Steps: []testfakes.RuntimeStep{
		testfakes.Emit(app.RuntimeEvent{Kind: app.RuntimeMessage, Text: "hi"}),
	}})
	s := start(t, rt, t.TempDir())
	stop, err := s.Send(context.Background(), "go")
	if err != nil || stop != app.StopEndTurn {
		t.Fatalf("Send = %q, %v; want end_turn", stop, err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := drain(s)
	if len(got) != 2 || got[0].Kind != app.RuntimeMessage || got[1].Kind != app.RuntimeStop || got[1].Stop != app.StopEndTurn {
		t.Errorf("events = %+v, want message then stop(end_turn)", got)
	}
	if prompts := rt.Session(0).Prompts(); len(prompts) != 1 || prompts[0] != "go" {
		t.Errorf("prompts = %v, want [go]", prompts)
	}
}

func TestRuntimePermissionAnswers(t *testing.T) {
	tests := []struct {
		name        string
		option      string
		cancelOnRej bool
		wantStop    app.StopReason
		wantWritten bool
	}{
		{"allowed", "allow", false, app.StopEndTurn, true},
		{"rejected and the turn goes on", "reject", false, app.StopEndTurn, false},
		{"rejected and the turn is cancelled", "reject", true, app.StopCancelled, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			rt := testfakes.NewRuntime("fake", testfakes.RuntimeScript{
				CancelOnReject: tc.cancelOnRej,
				Steps: []testfakes.RuntimeStep{ask("r1"), testfakes.Do(func(workdir string, allowed bool) {
					if allowed {
						_ = os.WriteFile(filepath.Join(workdir, "out.txt"), []byte("x"), 0o600)
					}
				})},
			})
			s := start(t, rt, dir)
			type res struct {
				stop app.StopReason
				err  error
			}
			done := make(chan res, 1)
			go func() {
				stop, err := s.Send(context.Background(), "go")
				done <- res{stop, err}
			}()
			ev := <-s.Events()
			if ev.Kind != app.RuntimePermissionRequest {
				t.Fatalf("first event = %s, want permission_request", ev.Kind)
			}
			select {
			case <-done:
				t.Fatal("Send returned before the request was answered")
			case <-time.After(20 * time.Millisecond):
			}
			if err := s.Approve(context.Background(), "r1", tc.option); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil || r.stop != tc.wantStop {
				t.Errorf("Send = %q, %v; want %q", r.stop, r.err, tc.wantStop)
			}
			if _, err := os.Stat(filepath.Join(dir, "out.txt")); (err == nil) != tc.wantWritten {
				t.Errorf("file written = %v, want %v", err == nil, tc.wantWritten)
			}
			calls := rt.Session(0).Approvals()
			if len(calls) != 1 || calls[0].RequestID != "r1" || calls[0].OptionID != tc.option {
				t.Errorf("approvals = %+v, want one call r1/%s", calls, tc.option)
			}
		})
	}
}

func TestRuntimeApproveUnknownRequest(t *testing.T) {
	s := start(t, testfakes.NewRuntime("fake"), t.TempDir())
	if err := s.Approve(context.Background(), "nope", "allow"); err == nil {
		t.Error("Approve of an unknown request succeeded, want error")
	}
}

func TestRuntimeCrashClosesSession(t *testing.T) {
	rt := testfakes.NewRuntime("fake", testfakes.RuntimeScript{Steps: []testfakes.RuntimeStep{
		testfakes.Emit(app.RuntimeEvent{Kind: app.RuntimeMessage, Text: "a"}), testfakes.Crash(),
	}})
	s := start(t, rt, t.TempDir())
	_, err := s.Send(context.Background(), "go")
	if !errors.Is(err, testfakes.ErrRuntimeCrashed) {
		t.Fatalf("Send error = %v, want ErrRuntimeCrashed", err)
	}
	if got := drain(s); len(got) != 1 || got[0].Text != "a" {
		t.Errorf("events = %+v, want only the message before the crash", got)
	}
}

func TestRuntimeStopEndsHangingTurn(t *testing.T) {
	rt := testfakes.NewRuntime("fake", testfakes.RuntimeScript{Steps: []testfakes.RuntimeStep{testfakes.Hang()}})
	s := start(t, rt, t.TempDir())
	type res struct {
		stop app.StopReason
		err  error
	}
	done := make(chan res, 1)
	go func() {
		stop, err := s.Send(context.Background(), "go")
		done <- res{stop, err}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.stop != app.StopCancelled {
			t.Errorf("Send = %q, %v; want cancelled", r.stop, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Send did not return after Stop")
	}
	if !rt.Session(0).Stopped() {
		t.Error("Stopped() = false")
	}
}

func TestRuntimeScriptExhausted(t *testing.T) {
	s := start(t, testfakes.NewRuntime("fake"), t.TempDir())
	if _, err := s.Send(context.Background(), "go"); err == nil {
		t.Error("Send with no script succeeded, want error")
	}
}
