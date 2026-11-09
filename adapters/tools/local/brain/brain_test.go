package brain_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/brain"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

func call(t *testing.T, r *local.Registry, ctx context.Context, name, args string) task.ToolResult {
	t.Helper()
	res, err := r.Call(ctx, task.ToolCall{ID: "c", Name: name, Args: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func TestSpecs(t *testing.T) {
	r := local.New()
	brain.Register(r, &testfakes.Brain{})
	got := map[string]task.ToolSpec{}
	for _, s := range r.List([]string{"brain.recall", "brain.remember", "brain.forget"}) {
		got[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("specs = %v", got)
	}
	if got["brain.recall"].Write || !got["brain.remember"].Write || !got["brain.forget"].Write {
		t.Errorf("write flags wrong: %+v", got)
	}
	for name, s := range got {
		if s.Untrusted {
			t.Errorf("%s must not be untrusted: brain content is information", name)
		}
	}
}

func TestRememberUsesKernelSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ctxSource  string
		args       string
		wantSource string
		unverified bool
		isErr      bool
	}{
		{"owner", "owner/t1", `{"text":"x","source":"owner/other"}`, "owner/t1", false, false},
		{"auto is unverified", "auto/t1", `{"text":"x","source":"owner/t1"}`, "auto/t1", true, false},
		{"no source refuses", "", `{"text":"x","source":"owner/t1"}`, "", false, true},
		{"empty text refuses", "owner/t1", `{"text":"  "}`, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &testfakes.Brain{}
			r := local.New()
			brain.Register(r, b)
			ctx := context.Background()
			if tc.ctxSource != "" {
				ctx = app.WithBrainSource(ctx, tc.ctxSource)
			}
			res := call(t, r, ctx, "brain.remember", tc.args)
			if res.IsError != tc.isErr {
				t.Fatalf("result = %+v, isErr want %v", res, tc.isErr)
			}
			if tc.isErr {
				if len(b.Remembered) != 0 {
					t.Fatalf("wrote despite refusal: %+v", b.Remembered)
				}
				return
			}
			if len(b.Remembered) != 1 || b.Remembered[0].Source != tc.wantSource || b.Remembered[0].Unverified != tc.unverified {
				t.Fatalf("remembered = %+v", b.Remembered)
			}
		})
	}
}

func TestRecallClampsLimitAndStripsInvisible(t *testing.T) {
	b := &testfakes.Brain{Items: []app.BrainItem{{Slug: "a", Text: "x\u200by", Source: "import/mybook"}}}
	r := local.New()
	brain.Register(r, b)
	res := call(t, r, context.Background(), "brain.recall", `{"query":"q","limit":99}`)
	if b.Limits[0] != 10 {
		t.Errorf("limit = %d, want clamped to 10", b.Limits[0])
	}
	if strings.Contains(res.Content, "\u200b") || !strings.Contains(res.Content, `"text":"xy"`) {
		t.Errorf("content = %s", res.Content)
	}
	call(t, r, context.Background(), "brain.recall", `{"query":"q"}`)
	if b.Limits[1] != 5 {
		t.Errorf("default limit = %d, want 5", b.Limits[1])
	}
	if res := call(t, r, context.Background(), "brain.recall", `{}`); !res.IsError {
		t.Error("empty query should be refused")
	}
}

func TestRememberPassesSlugThrough(t *testing.T) {
	b := &testfakes.Brain{}
	r := local.New()
	brain.Register(r, b)
	call(t, r, app.WithBrainSource(context.Background(), "auto/c1"), "brain.remember", `{"slug":"inbox/a","text":"x"}`)
	if len(b.Remembered) != 1 || b.Remembered[0].Slug != "inbox/a" {
		t.Fatalf("remembered = %+v, want slug inbox/a", b.Remembered)
	}
}

func TestForget(t *testing.T) {
	b := &testfakes.Brain{}
	r := local.New()
	brain.Register(r, b)
	if res := call(t, r, context.Background(), "brain.forget", `{"slug":"fake-1"}`); res.IsError || len(b.Forgotten) != 1 {
		t.Fatalf("res = %+v forgotten %v", res, b.Forgotten)
	}
	if res := call(t, r, context.Background(), "brain.forget", `{}`); !res.IsError {
		t.Error("empty slug should be refused")
	}
}
