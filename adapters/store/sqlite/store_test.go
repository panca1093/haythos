package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/panca/haythos/core/task"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := t.TempDir() + "/h.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestAppendAssignsCursorAndPerTaskSeq(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	var seqs []int
	for _, id := range []string{"a", "b", "a", "b", "a"} {
		e, err := s.Append(ctx, id, task.EvMessage, map[string]string{"x": "y"})
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, e.Seq)
	}
	if want := []int{1, 1, 2, 2, 3}; !equal(seqs, want) {
		t.Errorf("seqs = %v, want %v", seqs, want)
	}
	a, _ := s.Load(ctx, "a")
	if len(a) != 3 || a[2].Seq != 3 {
		t.Errorf("load a = %+v", a)
	}
	since, _ := s.Since(ctx, a[0].Cursor)
	if len(since) != 4 || since[0].Cursor <= a[0].Cursor {
		t.Errorf("since = %+v", since)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTasksInUsesLatestState(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	for _, st := range []task.State{task.Pending, task.Running} {
		_, _ = s.Append(ctx, "t1", task.EvState, task.StateChange{State: st})
	}
	_, _ = s.Append(ctx, "t2", task.EvState, task.StateChange{State: task.Pending})
	running, err := s.TasksIn(ctx, task.Running)
	if err != nil || len(running) != 1 || running[0] != "t1" {
		t.Errorf("running = %v err=%v", running, err)
	}
}

func TestLedgerRoundTripAndPersistence(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	e := task.LedgerEntry{IdemKey: "T/c1", TaskID: "T", Tool: "w", Target: "#x", ArgsHash: "h", State: task.LedgerDispatching, UpdatedAt: time.Now()}
	if err := s.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	e.State, e.Result = task.LedgerSucceeded, "ok"
	if err := s.Put(ctx, e); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	got, ok, err := s2.Get(ctx, "T/c1")
	if err != nil || !ok || got.State != task.LedgerSucceeded || got.Result != "ok" {
		t.Errorf("got %+v ok=%v err=%v", got, ok, err)
	}
	if rows, _ := s2.InState(ctx, task.LedgerDispatching); len(rows) != 0 {
		t.Errorf("dispatching rows = %v", rows)
	}
}
