package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panca/haythos/core/task"
)

func captureBody(t *testing.T, history []task.Message) string {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()
	if _, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	return body
}

func history(r ...task.ToolResult) []task.Message {
	return []task.Message{
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "a", Name: "fs.read", Args: []byte(`{}`)}}},
		{Role: task.RoleTool, Results: r},
		{Role: task.RoleUser, Text: "next"},
	}
}

func TestImagesFollowToolMessagesAsOneUserMessage(t *testing.T) {
	got := captureBody(t, history(
		task.ToolResult{CallID: "a", Content: "see", Images: []task.Image{{MediaType: "image/png", Data: []byte("abc")}, {MediaType: "image/jpeg", Data: []byte("xyz")}}},
		task.ToolResult{CallID: "b", Content: "plain"},
		task.ToolResult{CallID: "c", Content: "more", Images: []task.Image{{MediaType: "image/png", Data: []byte("abc")}}},
	))
	want := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"fs__read","arguments":"{}"}}]},` +
		`{"role":"tool","content":"see","tool_call_id":"a"},{"role":"tool","content":"plain","tool_call_id":"b"},{"role":"tool","content":"more","tool_call_id":"c"},` +
		`{"role":"user","content":[{"type":"text","text":"images from tool call a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YWJj"}},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,eHl6"}},{"type":"text","text":"images from tool call c"},{"type":"image_url","image_url":{"url":"data:image/png;base64,YWJj"}}]},` +
		`{"role":"user","content":"next"}]}`
	if got != want {
		t.Errorf("body = %s\nwant %s", got, want)
	}
}

func TestHistoryWithoutImagesIsUnchanged(t *testing.T) {
	got := captureBody(t, history(task.ToolResult{CallID: "a", Content: "see", IsError: true}))
	want := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"fs__read","arguments":"{}"}}]},{"role":"tool","content":"error: see","tool_call_id":"a"},{"role":"user","content":"next"}]}`
	if got != want {
		t.Errorf("body = %s\nwant %s", got, want)
	}
}
