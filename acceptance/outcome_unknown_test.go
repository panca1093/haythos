package acceptance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

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
