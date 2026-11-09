package acceptance_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/app/gateway"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func TestTickResendsUndeliveredApproval(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, callTurn("c1", "msg.send", sendArgs))
	r.notifier.FailNext = 1
	id := r.create(testJob, "tell ops hello at #ops")
	if err := r.app.RunTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := len(r.notifier.Of(app.NoticeApproval)); got != 0 {
		t.Fatalf("approval notices before tick = %d, want 0 (publish failed)", got)
	}
	for i := 0; i < 2; i++ {
		if err := r.app.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(r.notifier.Of(app.NoticeApproval)); got != 1 {
		t.Errorf("approval notices after ticks = %d, want exactly 1", got)
	}
	if got := r.snap(id).State; got != task.WaitingApproval {
		t.Errorf("state = %s, want waiting_approval", got)
	}
}

func TestTickExpiresStaleApproval(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, callTurn("c1", "msg.send", sendArgs))
	id := r.create(testJob, "tell ops hello at #ops")
	if err := r.app.RunTask(ctx, id); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(30 * time.Minute)
	if err := r.app.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.snap(id).State; got != task.WaitingApproval {
		t.Fatalf("state before expiry = %s, want waiting_approval", got)
	}
	r.clock.Advance(time.Hour)
	if err := r.app.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	s := r.snap(id)
	if s.State != task.Failed || s.Reason != "approval expired" {
		t.Errorf("state=%s reason=%q, want failed/approval expired", s.State, s.Reason)
	}
	if got := r.tools.Count("msg.send"); got != 0 {
		t.Errorf("sends = %d, want 0", got)
	}
	if got := len(r.notifier.Of(app.NoticeState)); got != 1 {
		t.Errorf("failure notices = %d, want 1", got)
	}
}

// 2. Pending approvals survive restart and are void on changed args or expiry.
func TestPendingApprovalAcrossRestart(t *testing.T) {
	ctx := context.Background()
	pending := func(t *testing.T) (*rig, string, app.ApprovalRequest) {
		r := newRig(t, callTurn("c1", "msg.send", sendArgs))
		id := r.create(testJob, "tell ops hello at #ops")
		if err := r.app.RunTask(ctx, id); err != nil {
			t.Fatal(err)
		}
		ap := r.lastApproval()
		r.boot(finalTurn("sent"))
		r.start()
		return r, id, ap
	}

	t.Run("nothing executes before approval", func(t *testing.T) {
		r, id, _ := pending(t)
		if got := r.tools.Count("msg.send"); got != 0 {
			t.Errorf("sends = %d, want 0", got)
		}
		if got := r.snap(id).State; got != task.WaitingApproval {
			t.Errorf("state = %s, want waiting_approval", got)
		}
	})

	t.Run("valid approval executes once", func(t *testing.T) {
		r, id, ap := pending(t)
		if err := r.app.HandleApproval(ctx, id, ap.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
		if got := r.tools.Count("msg.send"); got != 1 {
			t.Errorf("sends = %d, want 1", got)
		}
		if got := r.snap(id).State; got != task.Done {
			t.Errorf("state = %s, want done", got)
		}
	})

	t.Run("expired approval is void", func(t *testing.T) {
		r, id, ap := pending(t)
		r.clock.Advance(2 * time.Hour)
		if err := r.app.HandleApproval(ctx, id, ap.Approval.ID, true); err != nil {
			t.Fatal(err)
		}
		if got := r.tools.Count("msg.send"); got != 0 {
			t.Errorf("sends = %d, want 0", got)
		}
		if !hasErrorResultContaining(r.snap(id), "void") {
			t.Error("history has no error tool result mentioning void")
		}
	})

	t.Run("gateway voids on args and policy version mismatch", func(t *testing.T) {
		r := newRig(t)
		gw := gateway.New(gateway.Deps{
			Tools: r.tools, Ledger: r.store, Vault: testfakes.Vault{}, Policy: testPolicy,
			Clock: r.clock.Now, ApprovalTTL: time.Hour, Paused: func() bool { return false },
		})
		approved := policy.Approval{
			ID: "ap", Tool: "msg.send", Target: "#ops", ArgsHash: policy.ArgsHash(json.RawMessage(sendArgs)),
			Expiry: r.clock.Now().Add(time.Hour), PolicyVersion: "v1",
		}
		changed := task.ToolCall{ID: "c1", Name: "msg.send", Args: json.RawMessage(`{"to":"#ops","text":"hello, and also this"}`)}
		_, err := gw.Execute(ctx, gateway.Request{TaskID: "T", Call: changed, Allowlist: testJob.Tools, Approval: &approved})
		if !errors.Is(err, policy.ErrApprovalVoid) {
			t.Errorf("changed args: err = %v, want ErrApprovalVoid", err)
		}
		stale := approved
		stale.PolicyVersion = "v0"
		same := task.ToolCall{ID: "c2", Name: "msg.send", Args: json.RawMessage(sendArgs)}
		_, err = gw.Execute(ctx, gateway.Request{TaskID: "T", Call: same, Allowlist: testJob.Tools, Approval: &stale})
		if !errors.Is(err, policy.ErrApprovalVoid) {
			t.Errorf("policy version: err = %v, want ErrApprovalVoid", err)
		}
		if got := r.tools.Count("msg.send"); got != 0 {
			t.Errorf("sends = %d, want 0", got)
		}
	})
}
