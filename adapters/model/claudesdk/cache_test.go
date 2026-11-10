package claudesdk

import (
	"strings"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestContextSummaryStaysOutOfSystemPrompt(t *testing.T) {
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleSystem, Text: "the summary", Context: true, Kind: task.KindSummary},
		{Role: task.RoleUser, Text: "hi"},
	}
	if sys := systemPrompt(history); strings.Contains(sys, "the summary") || !strings.HasPrefix(sys, "goal") {
		t.Errorf("system = %q", sys)
	}
	_, conv, _ := strings.Cut(renderPrompt(history, nil), "Conversation:\n")
	if !strings.HasPrefix(conv, "the summary\n[user] hi") {
		t.Errorf("conversation = %q", conv)
	}
}

func TestOtherContextSystemMessagesStayInSystemPrompt(t *testing.T) {
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleSystem, Text: "recalled memory", Context: true},
		{Role: task.RoleUser, Text: "hi"},
	}
	if sys := systemPrompt(history); !strings.HasPrefix(sys, "goal\n\nrecalled memory") {
		t.Errorf("system = %q", sys)
	}
	_, conv, _ := strings.Cut(renderPrompt(history, nil), "Conversation:\n")
	if strings.Contains(conv, "recalled memory") {
		t.Errorf("conversation = %q", conv)
	}
}

func TestToolsRenderedSortedByName(t *testing.T) {
	a := renderPrompt(nil, []task.ToolSpec{{Name: "b.x"}, {Name: "a.x"}})
	b := renderPrompt(nil, []task.ToolSpec{{Name: "a.x"}, {Name: "b.x"}})
	if a != b || strings.Index(a, "- a.x") > strings.Index(a, "- b.x") {
		t.Errorf("prompts differ or unsorted:\n%s\n%s", a, b)
	}
}
