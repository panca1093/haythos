package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestStepMapsRequestAndResponse(t *testing.T) {
	var got request
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("authorization"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"vmhealth__read","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":9,"completion_tokens":4}}`))
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", APIKey: "k", Model: "gpt"})
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "a", Name: "vmhealth.read", Args: json.RawMessage(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "a", Content: "boom", IsError: true}}},
	}
	turn, err := a.Step(context.Background(), history, []task.ToolSpec{{Name: "vmhealth.read", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer k" || path != "/v1/chat/completions" {
		t.Errorf("auth=%q path=%q", auth, path)
	}
	if len(got.Messages) != 4 || got.Messages[2].ToolCalls[0].Function.Name != "vmhealth__read" || *got.Messages[3].Content != "error: boom" {
		t.Errorf("request = %+v", got)
	}
	if len(turn.Calls) != 1 || turn.Calls[0].Name != "vmhealth.read" || string(turn.Calls[0].Args) != `{"a":1}` || turn.Usage.OutputTokens != 4 {
		t.Errorf("turn = %+v", turn)
	}
}
