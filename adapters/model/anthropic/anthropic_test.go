package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestStepMapsRequestAndResponse(t *testing.T) {
	var got request
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"tu1","name":"vmhealth__read","input":{"x":1}}],"usage":{"input_tokens":11,"output_tokens":7}}`))
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Calls: []task.ToolCall{{ID: "a", Name: "vmhealth.read", Args: json.RawMessage(`{}`)}, {ID: "b", Name: "vmhealth.read", Args: json.RawMessage(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "a", Content: "ra"}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "b", Content: "rb", IsError: true}}},
	}
	turn, err := a.Step(context.Background(), history, []task.ToolSpec{{Name: "vmhealth.read", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Get("x-api-key") != "k" || hdr.Get("anthropic-version") == "" {
		t.Errorf("headers = %v", hdr)
	}
	if got.Tools[0].Name != "vmhealth__read" || len(got.Messages) != 3 {
		t.Errorf("request = %+v", got)
	}
	if last := got.Messages[2]; last.Role != "user" || len(last.Content) != 2 || !last.Content[1].IsError {
		t.Errorf("tool results not merged into one user message: %+v", last)
	}
	if turn.Text != "checking" || len(turn.Calls) != 1 || turn.Calls[0].Name != "vmhealth.read" || turn.Calls[0].ID != "tu1" || turn.Usage.InputTokens != 11 {
		t.Errorf("turn = %+v", turn)
	}
}

func TestStepReportsHTTPErrorWithoutKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL, APIKey: "sekrit", Model: "m"}).Step(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "sekrit") {
		t.Errorf("err = %v", err)
	}
}
