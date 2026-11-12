package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panca/haythos/core/task"
)

func captureBody(t *testing.T, history []task.Message) string {
	t.Helper()
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"content":[],"usage":{}}`))
	}))
	defer srv.Close()
	if _, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	return body
}

func toolHistory(images []task.Image) []task.Message {
	return []task.Message{
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "a", Name: "fs.read", Args: []byte(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "a", Content: "see", IsError: true, Images: images}}},
	}
}

func TestToolResultWithImagesIsContentArray(t *testing.T) {
	got := captureBody(t, toolHistory([]task.Image{{MediaType: "image/png", Data: []byte("abc")}, {MediaType: "image/jpeg", Data: []byte("xyz")}}))
	want := `{"type":"tool_result","tool_use_id":"a","content":[{"type":"text","text":"see"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"eHl6"}}],"is_error":true,"cache_control":{"type":"ephemeral"}}`
	if !strings.Contains(got, want) {
		t.Errorf("body = %s\nwant substring %s", got, want)
	}
}

func TestToolResultWithoutImagesStaysString(t *testing.T) {
	got := captureBody(t, toolHistory(nil))
	want := `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"fs__read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"see","is_error":true,"cache_control":{"type":"ephemeral"}}]}]}`
	if got != want {
		t.Errorf("body = %s\nwant %s", got, want)
	}
}
