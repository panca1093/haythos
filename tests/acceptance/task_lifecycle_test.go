package acceptance_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/panca/haythos/adapters/model/claudesdk"
	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

// 4. claudesdk: invalid tool calls never execute, one repair retry, no SDK built-in tools.
type scriptedTransport struct {
	replies []string
	reqs    []claudesdk.StepRequest
}

func (s *scriptedTransport) Step(ctx context.Context, req claudesdk.StepRequest) (claudesdk.StepResponse, error) {
	s.reqs = append(s.reqs, req)
	i := len(s.reqs) - 1
	if i >= len(s.replies) {
		i = len(s.replies) - 1
	}
	return claudesdk.StepResponse{Text: s.replies[i], Usage: task.Usage{InputTokens: 10, OutputTokens: 5}}, nil
}

func TestClaudeSDKValidatesToolCalls(t *testing.T) {
	ctx := context.Background()
	history := []task.Message{{Role: task.RoleSystem, Text: "You are Alfred."}, {Role: task.RoleUser, Text: "tell ops hello"}}
	tools := []task.ToolSpec{specs[3]}
	const bad = `{"text":"","tool_calls":[{"name":"msg.send","args":{"to":5}}]}`
	const good = `{"text":"","tool_calls":[{"name":"msg.send","args":{"to":"#ops","text":"hello"}}]}`

	t.Run("invalid twice fails after one repair", func(t *testing.T) {
		tr := &scriptedTransport{replies: []string{"not json at all", bad}}
		_, err := claudesdk.New(tr).Step(ctx, history, tools)
		if !errors.Is(err, claudesdk.ErrInvalidOutput) {
			t.Fatalf("err = %v, want ErrInvalidOutput", err)
		}
		if len(tr.reqs) != 2 {
			t.Errorf("sidecar calls = %d, want 2 (one repair)", len(tr.reqs))
		}
	})

	t.Run("unknown tool is invalid", func(t *testing.T) {
		tr := &scriptedTransport{replies: []string{`{"text":"","tool_calls":[{"name":"shell.exec","args":{}}]}`}}
		if _, err := claudesdk.New(tr).Step(ctx, history, tools); !errors.Is(err, claudesdk.ErrInvalidOutput) {
			t.Fatalf("err = %v, want ErrInvalidOutput", err)
		}
	})

	t.Run("repair succeeds", func(t *testing.T) {
		tr := &scriptedTransport{replies: []string{bad, good}}
		turn, err := claudesdk.New(tr).Step(ctx, history, tools)
		if err != nil {
			t.Fatal(err)
		}
		if len(turn.Calls) != 1 || turn.Calls[0].Name != "msg.send" || turn.Calls[0].ID == "" {
			t.Errorf("calls = %+v", turn.Calls)
		}
		if len(tr.reqs) != 2 {
			t.Errorf("sidecar calls = %d, want 2", len(tr.reqs))
		}
	})

	t.Run("built-in tools never enabled and tool list in prompt", func(t *testing.T) {
		tr := &scriptedTransport{replies: []string{bad, good}}
		if _, err := claudesdk.New(tr).Step(ctx, history, tools); err != nil {
			t.Fatal(err)
		}
		for i, req := range tr.reqs {
			if req.Tools == nil || len(req.Tools) != 0 {
				t.Errorf("request %d: Tools = %#v, want empty non-nil", i, req.Tools)
			}
			if !strings.Contains(req.Prompt, "msg.send") {
				t.Errorf("request %d: prompt does not list msg.send", i)
			}
		}
	})

	t.Run("task fails with clear reason and nothing executes", func(t *testing.T) {
		r := newRig(t)
		r.models = map[string]app.Model{"default": claudesdk.New(&scriptedTransport{replies: []string{bad}})}
		r.boot()
		id := r.create(testJob, "tell ops hello")
		if err := r.app.RunTask(ctx, id); err != nil {
			t.Fatal(err)
		}
		s := r.snap(id)
		if s.State != task.Failed || !strings.Contains(s.Reason, "invalid model output") {
			t.Errorf("state=%s reason=%q", s.State, s.Reason)
		}
		if got := r.tools.Count("msg.send"); got != 0 {
			t.Errorf("sends = %d, want 0", got)
		}
	})
}

// 5. A task killed mid-running resumes to the same state and history.
func TestResumeFromEventLog(t *testing.T) {
	ctx := context.Background()
	script := func() []task.AssistantTurn {
		return []task.AssistantTurn{callTurn("c1", "vm.read", `{}`), callTurn("c2", "vm.read", `{}`), finalTurn("all normal")}
	}

	ref := newRig(t, script()...)
	refID := ref.create(testJob, "check the vm")
	if err := ref.app.RunTask(ctx, refID); err != nil {
		t.Fatal(err)
	}
	want := ref.snap(refID)
	if want.State != task.Done {
		t.Fatalf("reference run state = %s", want.State)
	}

	r := newRig(t, script()...)
	hits := 0
	r.hook = func(p string) error {
		if p == "app.after_tool_result" {
			hits++
			if hits == 1 {
				return errCrash
			}
		}
		return nil
	}
	id := r.create(testJob, "check the vm")
	if err := r.app.RunTask(ctx, id); !errors.Is(err, errCrash) {
		t.Fatalf("run: want crash, got %v", err)
	}
	if got := r.snap(id).State; got != task.Running {
		t.Fatalf("state at crash = %s, want running", got)
	}

	r.hook = nil
	r.boot(script()[1:]...)
	r.start()

	got := r.snap(id)
	if got.State != task.Done {
		t.Errorf("state = %s, want done", got.State)
	}
	if diff := cmp.Diff(want.History, got.History); diff != "" {
		t.Errorf("history differs from uninterrupted run (-want +got):\n%s", diff)
	}
	if n := r.tools.Count("vm.read"); n != 2 {
		t.Errorf("vm.read calls = %d, want 2 (no replay of finished calls)", n)
	}
}

// 6. Budget overrun ends failed with reason budget and notifies once.
func TestBudget(t *testing.T) {
	ctx := context.Background()
	loop := func(usage task.Usage) []task.AssistantTurn {
		var out []task.AssistantTurn
		for _, id := range []string{"c1", "c2", "c3", "c4"} {
			turn := callTurn(id, "vm.read", `{}`)
			turn.Usage = usage
			out = append(out, turn)
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		budget task.Budget
		usage  task.Usage
	}{
		{"tokens", task.Budget{Tokens: 1000, Steps: 50}, task.Usage{InputTokens: 500, OutputTokens: 100}},
		{"steps", task.Budget{Tokens: 1000000, Steps: 2}, task.Usage{InputTokens: 1, OutputTokens: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, loop(tc.usage)...)
			job := testJob
			job.Budget = tc.budget
			id := r.create(job, "check the vm")
			if err := r.app.RunTask(ctx, id); err != nil {
				t.Fatal(err)
			}
			s := r.snap(id)
			if s.State != task.Failed || s.Reason != "budget" {
				t.Fatalf("state=%s reason=%q, want failed/budget", s.State, s.Reason)
			}
			if err := r.app.RunTask(ctx, id); err != nil {
				t.Fatal(err)
			}
			r.boot()
			r.start()
			var failed int
			for _, n := range r.notifier.Of(app.NoticeState) {
				if n.State == task.Failed {
					failed++
				}
			}
			if failed != 1 {
				t.Errorf("failure notices = %d, want exactly 1", failed)
			}
		})
	}
}

// 7. /pause stops writes immediately, including approved but undispatched ones.
func TestPause(t *testing.T) {
	ctx := context.Background()
	waiting := func(t *testing.T) (*rig, string, app.ApprovalRequest) {
		r := newRig(t, callTurn("c1", "msg.send", sendArgs))
		id := r.create(testJob, "tell ops hello at #ops")
		if err := r.app.RunTask(ctx, id); err != nil {
			t.Fatal(err)
		}
		return r, id, r.lastApproval()
	}

	t.Run("pause before approval, survives restart", func(t *testing.T) {
		r, id, ap := waiting(t)
		if err := r.app.Pause(ctx); err != nil {
			t.Fatal(err)
		}
		r.boot(finalTurn("could not send"))
		r.start()
		if err := r.app.HandleApproval(ctx, id, ap.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
		if got := r.tools.Count("msg.send"); got != 0 {
			t.Errorf("sends while paused = %d, want 0", got)
		}
		if !hasErrorResultContaining(r.snap(id), "paused") {
			t.Error("history has no error tool result mentioning paused")
		}
	})

	t.Run("pause between approval and dispatch", func(t *testing.T) {
		r, id, ap := waiting(t)
		r.boot(finalTurn("could not send"))
		r.start()
		r.hook = func(p string) error {
			if p == "gateway.before_dispatch" {
				return r.app.Pause(ctx)
			}
			return nil
		}
		if err := r.app.HandleApproval(ctx, id, ap.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
		if got := r.tools.Count("msg.send"); got != 0 {
			t.Errorf("sends = %d, want 0", got)
		}
	})

	t.Run("resume allows writes again", func(t *testing.T) {
		r, id, ap := waiting(t)
		if err := r.app.Pause(ctx); err != nil {
			t.Fatal(err)
		}
		if err := r.app.Resume(ctx); err != nil {
			t.Fatal(err)
		}
		r.boot(finalTurn("sent"))
		r.start()
		if err := r.app.HandleApproval(ctx, id, ap.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
		if got := r.tools.Count("msg.send"); got != 1 {
			t.Errorf("sends = %d, want 1", got)
		}
	})
}

type hangingModel struct{}

func (hangingModel) Step(ctx context.Context, _ []task.Message, _ []task.ToolSpec) (task.AssistantTurn, error) {
	<-ctx.Done()
	return task.AssistantTurn{}, ctx.Err()
}

func TestHungModelStepFailsTheTask(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	notifier := &testfakes.Notifier{}
	a := app.New(app.Deps{
		Models: map[string]app.Model{"default": hangingModel{}}, DefaultJob: testJob,
		Tools: testfakes.NewTools(specs...), Events: st, Ledger: st, Notifier: notifier,
		Vault: testfakes.Vault{}, Policy: testPolicy, StepTimeout: 50 * time.Millisecond,
	})
	id, err := a.CreateTask(ctx, testJob, "hello")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- a.RunTask(ctx, id) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunTask still blocked on a hung model")
	}
	s, err := a.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != task.Failed || !strings.Contains(s.Reason, "timed out") {
		t.Errorf("state=%s reason=%q, want failed with timed out", s.State, s.Reason)
	}
	if got := len(notifier.Of(app.NoticeState)); got != 1 {
		t.Errorf("failure notices = %d, want 1", got)
	}
}
