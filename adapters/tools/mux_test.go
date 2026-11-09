package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

func TestMuxRoutesByToolName(t *testing.T) {
	a := testfakes.NewTools(task.ToolSpec{Name: "a.one"})
	b := testfakes.NewTools(task.ToolSpec{Name: "b.two"})
	b.ExistsFn = func(string, string, string) (bool, error) { return true, nil }
	m := Mux{a, b}

	if got := m.List([]string{"a.one", "b.two", "c.none"}); len(got) != 2 {
		t.Fatalf("list = %+v, want 2 specs", got)
	}
	if _, err := m.Call(context.Background(), task.ToolCall{ID: "1", Name: "b.two"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	if a.Count("a.one") != 0 || b.Count("b.two") != 1 {
		t.Errorf("calls a=%d b=%d, want b only", a.Count("a.one"), b.Count("b.two"))
	}
	if _, err := m.Call(context.Background(), task.ToolCall{ID: "2", Name: "c.none"}); err == nil {
		t.Error("expected an error for an unknown tool")
	}
	if ok, err := m.Exists(context.Background(), "b.two", "", "k"); err != nil || !ok {
		t.Errorf("exists = %v %v, want true", ok, err)
	}
	if _, err := m.Exists(context.Background(), "c.none", "", "k"); !errors.Is(err, app.ErrCannotVerify) {
		t.Errorf("exists on unknown tool err = %v, want ErrCannotVerify", err)
	}
}
