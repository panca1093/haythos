package claudesdk

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestStepRendersImagePlaceholdersInOrder(t *testing.T) {
	t.Parallel()
	red := task.Image{MediaType: "image/png", Data: []byte("red")}
	blue := task.Image{MediaType: "image/jpeg", Data: []byte("blue")}
	green := task.Image{MediaType: "image/png", Data: []byte("green")}
	history := []task.Message{
		{Role: task.RoleUser, Text: "look"},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "c1", Name: "file.read", Args: json.RawMessage(`{}`)}, {ID: "c2", Name: "file.read", Args: json.RawMessage(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{
			{CallID: "c1", Content: "two pages", Images: []task.Image{red, blue}},
			{CallID: "c2", Content: "text only"},
		}},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "c3", Name: "file.read", Args: json.RawMessage(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "c3", Content: "one", Images: []task.Image{green}}}},
	}
	tr := &captureTransport{}
	if _, err := New(tr).Step(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	req := tr.reqs[0]
	_, conv, _ := strings.Cut(req.Prompt, "Conversation:\n")
	want := "[user] look\n" +
		"[assistant called file.read id=c1] {}\n[assistant called file.read id=c2] {}\n" +
		"[tool result id=c1] two pages\n[image 1 from tool call c1]\n[image 2 from tool call c1]\n" +
		"[tool result id=c2] text only\n" +
		"[assistant called file.read id=c3] {}\n" +
		"[tool result id=c3] one\n[image 3 from tool call c3]\n"
	if conv != want {
		t.Errorf("conversation =\n%q\nwant\n%q", conv, want)
	}
	if len(req.Images) != 3 || !bytes.Equal(req.Images[0].Data, red.Data) || !bytes.Equal(req.Images[1].Data, blue.Data) || req.Images[1].MediaType != "image/jpeg" || !bytes.Equal(req.Images[2].Data, green.Data) {
		t.Errorf("images = %+v, want red, blue, green in placeholder order", req.Images)
	}
}

func TestImageFreePromptIsUnchanged(t *testing.T) {
	t.Parallel()
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Text: "checking", Calls: []task.ToolCall{{ID: "c1", Name: "vm.read", Args: json.RawMessage(`{"a":1}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "c1", Content: "ok"}, {CallID: "c2", Content: "bad", IsError: true}}},
	}
	tools := []task.ToolSpec{{Name: "vm.read", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	want := "Output format: reply with ONLY one JSON object, no prose, no code fences:\n" +
		`{"text": "<your reply to the user, may be empty when calling tools>", "tool_calls": [{"name": "<tool name>", "args": {...}}]}` +
		"\nOmit tool_calls (or leave it empty) when you are done.\n\nAvailable tools:\n" +
		"- vm.read: read\n  input schema: {\"type\":\"object\"}\n" +
		"\nConversation:\n[user] hi\n[assistant] checking\n[assistant called vm.read id=c1] {\"a\":1}\n[tool result id=c1] ok\n[tool error id=c2] bad\n"
	tr := &captureTransport{}
	if _, err := New(tr).Step(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	if tr.reqs[0].Prompt != want {
		t.Errorf("prompt =\n%q\nwant\n%q", tr.reqs[0].Prompt, want)
	}
	if tr.reqs[0].Images != nil {
		t.Errorf("images = %+v, want none", tr.reqs[0].Images)
	}
	raw, _ := json.Marshal(tr.reqs[0])
	if strings.Contains(string(raw), `"images"`) {
		t.Errorf("image-free request carries an images key: %s", raw)
	}
}
