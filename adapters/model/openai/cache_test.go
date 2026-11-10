package openai

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

func TestStepRendersSummaryAsUserMessageAndSortsTools(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{}}`))
	}))
	defer srv.Close()
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleSystem, Text: "the summary", Context: true},
	}
	tools := []task.ToolSpec{{Name: "b.tool"}, {Name: "a.tool"}}
	if _, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "system" || *got.Messages[0].Content != "goal" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if s := got.Messages[1]; s.Role != "user" || !strings.Contains(*s.Content, "the summary") {
		t.Errorf("summary = %+v", s)
	}
	if *got.Messages[2].Content != "hi" || got.Tools[0].Function.Name != "a__tool" {
		t.Errorf("request = %+v", got)
	}
}
