package tools

import (
	"testing"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/core/task"
)

func TestMuxListIsSortedByName(t *testing.T) {
	a := testfakes.NewTools(task.ToolSpec{Name: "z.last"}, task.ToolSpec{Name: "m.mid"})
	b := testfakes.NewTools(task.ToolSpec{Name: "a.first"})
	got := Mux{a, b}.List([]string{"z.last", "m.mid", "a.first"})
	if len(got) != 3 || got[0].Name != "a.first" || got[1].Name != "m.mid" || got[2].Name != "z.last" {
		t.Errorf("list = %+v", got)
	}
}
