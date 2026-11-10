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

func TestStepCachesPrefixAndKeepsSummaryOutOfSystem(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"content":[],"usage":{}}`))
	}))
	defer srv.Close()
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleSystem, Text: "the summary", Context: true, Kind: task.KindSummary},
		{Role: task.RoleUser, Text: "hi"},
	}
	tools := []task.ToolSpec{{Name: "b.tool", InputSchema: json.RawMessage(`{}`)}, {Name: "a.tool", InputSchema: json.RawMessage(`{}`)}}
	if _, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	if len(got.System) != 1 || got.System[0].Text != "goal" || got.System[0].CacheControl == nil || got.System[0].CacheControl.Type != "ephemeral" {
		t.Errorf("system = %+v", got.System)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "user" || !strings.Contains(got.Messages[0].Content[0].Text, "the summary") || got.Messages[1].Content[0].Text != "hi" {
		t.Fatalf("summary not first user content: %+v", got.Messages)
	}
	last := got.Messages[1].Content
	if last[len(last)-1].CacheControl == nil || got.Messages[0].Content[0].CacheControl != nil {
		t.Errorf("cache_control not only on the last block: %+v", got.Messages)
	}
	if got.Tools[0].Name != "a__tool" || got.Tools[1].Name != "b__tool" {
		t.Errorf("tools not sorted: %+v", got.Tools)
	}
}

func TestStepCountsCachedTokensInInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[],"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":7}}`))
	}))
	defer srv.Close()
	turn, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), []task.Message{{Role: task.RoleUser, Text: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (task.Usage{InputTokens: 3210, OutputTokens: 7, CacheReadTokens: 3000}); turn.Usage != want {
		t.Errorf("usage = %+v, want %+v", turn.Usage, want)
	}
}

func TestStepRendersRecallInPlaceOutOfSystem(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = w.Write([]byte(`{"content":[],"usage":{}}`))
	}))
	defer srv.Close()
	history := []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleSystem, Text: "recalled memory", Context: true},
	}
	if _, err := New(Config{BaseURL: srv.URL, Model: "m"}).Step(context.Background(), history, nil); err != nil {
		t.Fatal(err)
	}
	if len(got.System) != 1 || got.System[0].Text != "goal" || len(got.Messages) != 2 || got.Messages[1].Content[0].Text != "recalled memory" {
		t.Errorf("system = %+v messages = %+v", got.System, got.Messages)
	}
}
