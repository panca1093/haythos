package acceptance_test

import (
	"context"
	"testing"
	"time"

	"github.com/panca/haythos/app"
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
