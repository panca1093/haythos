package chatgpt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type fakeAPI struct {
	session   atomic.Value
	srv       *httptest.Server
	responses atomic.Int32
	body      atomic.Value
	auth      atomic.Value
}

// newFakeAPI serves /v1/responses with handler and a fixed /v1/models catalog.
func newFakeAPI(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		f.responses.Add(1)
		b, _ := io.ReadAll(r.Body)
		f.body.Store(b)
		f.auth.Store(r.Header.Get("Authorization"))
		f.session.Store(r.Header.Get("session-id"))
		handler(w, r)
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"hidden","display_name":"H","visibility":"hide"},{"slug":"gpt-test","display_name":"GPT Test","visibility":"list"}]}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		var typ struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &typ)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ.Type, e)
	}
}

const completed = `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":11,"output_tokens":5}}}`

func newAdapter(t *testing.T, api *fakeAPI, model string) *Adapter {
	t.Helper()
	auth := newFakeAuth(t)
	v := openVault(t, t.TempDir()+"/v.enc")
	seed(t, v, 30*time.Minute)
	return New(Config{Model: model, BaseURL: api.srv.URL + "/v1", HTTP: api.srv.Client(), Session: newTestSession(auth, v)})
}

var (
	history = []task.Message{
		{Role: task.RoleSystem, Text: "goal"},
		{Role: task.RoleUser, Text: "hi"},
		{Role: task.RoleAssistant, Text: "checking", Calls: []task.ToolCall{{ID: "c1", Name: "vmhealth.read", Args: json.RawMessage(`{}`)}}},
		{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "c1", Content: "boom", IsError: true}}},
	}
	tools = []task.ToolSpec{{Name: "vmhealth.read", Description: "read vm", InputSchema: json.RawMessage(`{"type":"object"}`)}}
)

func TestStepRequestShape(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { sse(w, completed) })
	if _, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	if api.auth.Load() != "Bearer at-old" {
		t.Errorf("auth = %v", api.auth.Load())
	}
	var got map[string]any
	if err := json.Unmarshal(api.body.Load().([]byte), &got); err != nil {
		t.Fatal(err)
	}
	want := `{"input":[{"content":"hi","role":"user"},{"content":"checking","role":"assistant"},` +
		`{"arguments":"{}","call_id":"c1","name":"vmhealth__read","namespace":"alfred","type":"function_call"},` +
		`{"call_id":"c1","output":"error: boom","type":"function_call_output"}],` +
		`"instructions":"goal","model":"gpt-x","store":false,"stream":true,` +
		`"tools":[{"description":"Alfred kernel tools. Every call is checked by policy and may need the user's approval.","name":"alfred",` +
		`"tools":[{"description":"read vm","name":"vmhealth__read","parameters":{"type":"object"},"type":"function"}],"type":"namespace"}]}`
	if b, _ := json.Marshal(got); string(b) != want {
		t.Errorf("request =\n%s\nwant\n%s", b, want)
	}
}

func TestStepSendsTheConversationAsPromptCacheKey(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { sse(w, completed) })
	a := newAdapter(t, api, "gpt-x")
	var keys []any
	for range 2 {
		if _, err := a.Step(app.WithCacheKey(context.Background(), "tui:abc"), history, tools); err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(api.body.Load().([]byte), &got); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, got["prompt_cache_key"])
		if h := api.session.Load(); h != "tui:abc" {
			t.Fatalf("session-id header = %v, want the conversation key: ChatGPT takes cache affinity from it", h)
		}
	}
	if keys[0] != "tui:abc" || keys[1] != "tui:abc" {
		t.Fatalf("prompt_cache_key per step = %v, want the conversation key on every step", keys)
	}
	if got := New(Config{Model: "gpt-6.1-sol"}).ContextWindow(); got != 1_000_000 {
		t.Fatalf("window = %d, want 1M", got)
	}
}

func TestStepParsesTextAndToolCalls(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w,
			`{"type":"response.output_text.delta","delta":"Hel"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"Hello"}]}}`,
			`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"call_9","namespace":"alfred","name":"vmhealth__read","arguments":"{\"a\":1}"}}`,
			completed)
	})
	turn, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "Hello" || len(turn.Calls) != 1 || turn.Calls[0].ID != "call_9" || turn.Calls[0].Name != "vmhealth.read" ||
		string(turn.Calls[0].Args) != `{"a":1}` || turn.Usage != (task.Usage{InputTokens: 11, OutputTokens: 5}) {
		t.Errorf("turn = %+v", turn)
	}
}

func TestStepReadsCachedTokens(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":500,"output_tokens":5,"input_tokens_details":{"cached_tokens":400}}}}`)
	})
	turn, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (task.Usage{InputTokens: 500, OutputTokens: 5, CacheReadTokens: 400}); turn.Usage != want {
		t.Errorf("usage = %+v, want %+v", turn.Usage, want)
	}
}

func TestStepUsesCompletedOutputWhenNoItemEvents(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}}`)
	})
	turn, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, nil)
	if err != nil || turn.Text != "done" {
		t.Fatalf("turn=%+v err=%v", turn, err)
	}
}

func TestStepForeignNamespace(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c","namespace":"other","name":"x","arguments":"{}"}}`, completed)
	})
	if _, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools); err == nil {
		t.Fatal("foreign namespace accepted")
	}
}

func TestStepUsageLimit(t *testing.T) {
	t.Parallel()
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"response.failed": func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"response.failed","response":{"status":"failed","error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}}`)
		},
		"http 429": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"subscription_sharing_usage_limit_exceeded","message":"limit"}}`)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newFakeAPI(t, h)
			_, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools)
			if !errors.Is(err, ErrUsageLimit) {
				t.Fatalf("err = %v, want ErrUsageLimit", err)
			}
			if n := api.responses.Load(); n != 1 {
				t.Errorf("requests = %d, want 1 (no retry, no fallback)", n)
			}
		})
	}
}

func TestStepUsageUnavailable(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.failed","response":{"error":{"code":"subscription_sharing_usage_unavailable","message":"try later"}}}`)
	})
	_, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools)
	if err == nil || !strings.Contains(err.Error(), "subscription_sharing_usage_unavailable") || api.responses.Load() != 1 {
		t.Fatalf("err = %v, requests = %d", err, api.responses.Load())
	}
}

func TestStepNoCompleted(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		sse(w, `{"type":"response.output_text.delta","delta":"Hel"}`)
	})
	if _, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools); err == nil {
		t.Fatal("stream without response.completed accepted")
	}
}

func TestStepCancel(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"type":"response.output_text.delta","delta":"Hel"}`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	a := newAdapter(t, api, "gpt-x")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := a.Step(ctx, history, tools)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}

func TestStepErrorHasNoToken(t *testing.T) {
	t.Parallel()
	cases := map[string]func(w http.ResponseWriter, r *http.Request){
		"http": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"code":"subscription_sharing_unsupported_capability","message":"bad header %s"}}`, r.Header.Get("Authorization"))
		},
		"stream": func(w http.ResponseWriter, r *http.Request) {
			sse(w, fmt.Sprintf(`{"type":"response.failed","response":{"error":{"code":"x","message":"bad header %s"}}}`, r.Header.Get("Authorization")))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newFakeAPI(t, h)
			_, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools)
			if err == nil || strings.Contains(err.Error(), "at-old") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestStepDefaultModel(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) { sse(w, completed) })
	if _, err := newAdapter(t, api, "").Step(context.Background(), history, tools); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(api.body.Load().([]byte), &got)
	if got.Model != "gpt-test" {
		t.Errorf("model = %q", got.Model)
	}
}

func TestToolNameMapping(t *testing.T) {
	t.Parallel()
	spec := func(n string) task.ToolSpec { return task.ToolSpec{Name: n} }
	t.Run("name with double underscore round-trips", func(t *testing.T) {
		t.Parallel()
		api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c","namespace":"alfred","name":"srv__foo__bar","arguments":"{}"}}`, completed)
		})
		turn, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, []task.ToolSpec{spec("srv.foo__bar")})
		if err != nil || len(turn.Calls) != 1 || turn.Calls[0].Name != "srv.foo__bar" {
			t.Fatalf("turn=%+v err=%v", turn, err)
		}
	})
	t.Run("unknown tool rejected", func(t *testing.T) {
		t.Parallel()
		api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			sse(w, `{"type":"response.output_item.done","item":{"type":"function_call","call_id":"c","namespace":"alfred","name":"nope","arguments":"{}"}}`, completed)
		})
		if _, err := newAdapter(t, api, "gpt-x").Step(context.Background(), history, tools); err == nil {
			t.Fatal("want error for a tool that was not offered")
		}
	})
	for name, specs := range map[string][]task.ToolSpec{
		"colon":     {spec("srv:tool")},
		"slash":     {spec("srv/tool")},
		"too long":  {spec(strings.Repeat("a", 65))},
		"collision": {spec("a.b"), spec("a__b")},
	} {
		t.Run("reject "+name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := buildRequest("m", nil, specs); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
