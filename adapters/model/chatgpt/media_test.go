package chatgpt

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/panca/haythos/core/task"
)

func inputJSON(t *testing.T, images []task.Image) string {
	t.Helper()
	h := []task.Message{
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "a", Name: "fs.read", Args: json.RawMessage(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "a", Content: "see", IsError: true, Images: images}}},
	}
	req, _, err := buildRequest("m", h, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(req.Input)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestToolOutputWithImagesIsPartsArray(t *testing.T) {
	got := inputJSON(t, []task.Image{{MediaType: "image/png", Data: []byte("abc")}, {MediaType: "image/jpeg", Data: []byte("xyz")}})
	want := `{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"error: see"},{"type":"input_image","image_url":"data:image/png;base64,YWJj"},{"type":"input_image","image_url":"data:image/jpeg;base64,eHl6"}]}`
	if !strings.HasSuffix(got, want+"]") {
		t.Errorf("input = %s\nwant tail %s", got, want)
	}
}

func TestToolOutputWithoutImagesStaysString(t *testing.T) {
	got := inputJSON(t, nil)
	want := `[{"role":"user","content":"hi"},{"type":"function_call","call_id":"a","namespace":"` + namespace + `","name":"fs__read","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"error: see"}]`
	if got != want {
		t.Errorf("input = %s\nwant %s", got, want)
	}
}
