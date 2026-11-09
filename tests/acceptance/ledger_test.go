package acceptance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

// 1. Crash after an external send but before success is logged.
func TestCrashAfterSendNoDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exists     func(tool, target, key string) (bool, error)
		wantLedger task.LedgerState
		wantAlert  int
	}{
		{"destination has it", func(string, string, string) (bool, error) { return true, nil }, task.LedgerSucceeded, 0},
		{"cannot verify", nil, task.LedgerOutcomeUnknown, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := newRig(t, callTurn("c1", "msg.send", sendArgs))
			r.tools.ExistsFn = tc.exists
			r.hook = func(p string) error {
				if p == "gateway.after_execute" {
					return errCrash
				}
				return nil
			}
			id := r.create(testJob, "tell ops hello at #ops")
			if err := r.app.RunTask(ctx, id); err != nil {
				t.Fatalf("run: %v", err)
			}
			ap := r.lastApproval()
			if err := r.app.HandleApproval(ctx, id, ap.Approval.ID, true); !errors.Is(err, errCrash) {
				t.Fatalf("approval: want crash, got %v", err)
			}
			if got := r.tools.Count("msg.send"); got != 1 {
				t.Fatalf("sends before restart = %d, want 1", got)
			}

			r.hook = nil
			r.boot(finalTurn("sent"))
			r.start()

			if got := r.tools.Count("msg.send"); got != 1 {
				t.Errorf("sends after restart = %d, want 1 (duplicate)", got)
			}
			states := ledgerStates(t, r.store)
			if states[tc.wantLedger] != 1 {
				t.Errorf("ledger states = %v, want one %s", states, tc.wantLedger)
			}
			if got := len(r.notifier.Of(app.NoticeAlert)); got != tc.wantAlert {
				t.Errorf("alerts = %d, want %d", got, tc.wantAlert)
			}
			if got := r.snap(id).State; got != task.Done {
				t.Errorf("task state = %s, want done", got)
			}
		})
	}
}

func TestOutcomeUnknownBlocksRetryToSameTarget(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, callTurn("c1", "msg.send", sendArgs))
	r.hook = func(p string) error {
		if p == "gateway.after_execute" {
			return errCrash
		}
		return nil
	}
	id := r.create(testJob, "tell ops hello at #ops")
	if err := r.app.RunTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := r.app.HandleApproval(ctx, id, r.lastApproval().Approval.ID, true); !errors.Is(err, errCrash) {
		t.Fatalf("approval: want crash, got %v", err)
	}

	r.hook = nil
	r.boot(callTurn("c2", "msg.send", `{"to":"#ops","text":"hello again"}`), finalTurn("gave up"))
	r.start()

	if got := r.tools.Count("msg.send"); got != 1 {
		t.Errorf("sends = %d, want 1", got)
	}
	if got := len(r.notifier.Of(app.NoticeApproval)); got != 1 {
		t.Errorf("approval requests = %d, want 1 (retry must not even ask)", got)
	}
	if !hasErrorResultContaining(r.snap(id), "unknown outcome") {
		t.Error("retry has no error result mentioning the unknown outcome")
	}
}

func TestAmbiguousWriteErrorIsOutcomeUnknown(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, callTurn("c1", "msg.send", sendArgs), finalTurn("could not tell"))
	r.tools.Handlers["msg.send"] = func(task.ToolCall) (task.ToolResult, error) {
		return task.ToolResult{}, errors.New("connection reset by peer")
	}
	id := r.create(testJob, "tell ops hello at #ops")
	if err := r.app.RunTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := r.app.HandleApproval(ctx, id, r.lastApproval().Approval.ID, true); err != nil {
		t.Fatal(err)
	}
	states := ledgerStates(t, r.store)
	if states[task.LedgerOutcomeUnknown] != 1 || states[task.LedgerFailed] != 0 {
		t.Errorf("ledger states = %v, want one outcome_unknown", states)
	}
	if got := len(r.notifier.Of(app.NoticeAlert)); got != 1 {
		t.Errorf("alerts = %d, want 1", got)
	}
}
