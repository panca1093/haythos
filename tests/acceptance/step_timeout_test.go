package acceptance_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

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
