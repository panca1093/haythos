package chatgpt

import (
	"strings"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestBuildRequestSummaryIsUserInputAndToolsSorted(t *testing.T) {
	h := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleSystem, Text: "the summary", Context: true},
		{Role: task.RoleUser, Text: "hi"},
	}
	req, _, err := buildRequest("m", h, []task.ToolSpec{{Name: "b.tool"}, {Name: "a.tool"}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Instructions != "goal" {
		t.Errorf("instructions = %q", req.Instructions)
	}
	if len(req.Input) != 2 || req.Input[0].Role != "user" || !strings.Contains(req.Input[0].Content, "the summary") || req.Input[1].Content != "hi" {
		t.Errorf("input = %+v", req.Input)
	}
	if got := req.Tools[0].Tools; got[0].Name != "a__tool" || got[1].Name != "b__tool" {
		t.Errorf("tools = %+v", got)
	}
}
