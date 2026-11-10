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
		{Role: task.RoleSystem, Text: "the summary", Context: true, Kind: task.KindSummary},
		{Role: task.RoleUser, Text: "hi"},
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

func TestStepReadsCachedTokensAndRendersRecallInPlace(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":500,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":400}}}`))
	}))
	defer srv.Close()
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleSystem, Text: "recalled memory", Context: true},
	}
	turn, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (task.Usage{InputTokens: 500, OutputTokens: 5, CacheReadTokens: 400}); turn.Usage != want {
		t.Errorf("usage = %+v, want %+v", turn.Usage, want)
	}
	if len(got.Messages) != 3 || got.Messages[2].Role != "user" || *got.Messages[2].Content != "recalled memory" {
		t.Errorf("messages = %+v", got.Messages)
	}
}
