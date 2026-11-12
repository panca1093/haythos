package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

const resultCap = 40 << 10

func bigRig(t *testing.T, content string, turns ...task.AssistantTurn) (*app.App, *testfakes.Model, *sqlite.Store) {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model := testfakes.NewModel(turns...)
	tools := testfakes.NewTools(task.ToolSpec{Name: "note.read", InputSchema: json.RawMessage(`{"type":"object"}`)})
	tools.Handlers["note.read"] = func(c task.ToolCall) (task.ToolResult, error) {
		return task.ToolResult{CallID: c.ID, Content: content}, nil
	}
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.read"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy:  policy.Policy{Tiers: map[string]policy.Tier{"note.read": policy.TierRead}},
		Clock:   time.Now,
		Windows: map[string]int{"default": 1_000_000},
	})
	return a, model, st
}

func lastResult(t *testing.T, h []task.Message) string {
	t.Helper()
	for i := len(h) - 1; i >= 0; i-- {
		if len(h[i].Results) > 0 {
			return h[i].Results[0].Content
		}
	}
	t.Fatal("no tool result in history")
	return ""
}

func checkCapped(t *testing.T, got string, origLen int) {
	t.Helper()
	if len(got) > resultCap || !utf8.ValidString(got) {
		t.Fatalf("result is %d bytes (valid UTF-8 %v), want at most %d", len(got), utf8.ValidString(got), resultCap)
	}
	if !strings.Contains(got, "bytes truncated…") || !strings.HasPrefix(got, "head") || !strings.HasSuffix(got, "tail") {
		t.Fatalf("want head, a truncation marker and tail; got prefix %q suffix %q", got[:20], got[len(got)-20:])
	}
	if origLen <= resultCap {
		t.Fatalf("test input too small: %d", origLen)
	}
}

func TestToolResultIsCappedWhenRecorded(t *testing.T) {
	t.Parallel()
	big := "head" + strings.Repeat("é", 50<<10) + "tail"
	a, model, _ := bigRig(t, big, readNote(10), used("done", 10))
	ask(t, a, "read it")
	checkCapped(t, lastResult(t, model.Histories[1]), len(big))
}

func TestSmallToolResultIsUnchanged(t *testing.T) {
	t.Parallel()
	a, model, _ := bigRig(t, "short", readNote(10), used("done", 10))
	ask(t, a, "read it")
	if got := lastResult(t, model.Histories[1]); got != "short" {
		t.Fatalf("result = %q, want unchanged", got)
	}
}

func TestLegacyLargeResultIsCappedOnReplay(t *testing.T) {
	t.Parallel()
	a, model, st := bigRig(t, "short", readNote(10), used("A1", 10), used("A2", 10))
	id := ask(t, a, "Q1")
	legacy := "head" + strings.Repeat("x", 64<<10) + "tail"
	if _, err := st.Append(context.Background(), id, task.EvMessage, task.Message{Role: task.RoleTool, Results: []task.ToolResult{{CallID: "old", Content: legacy}}}); err != nil {
		t.Fatal(err)
	}
	ask(t, a, "Q2")
	checkCapped(t, lastResult(t, model.Histories[2]), len(legacy))
}

type keyModel struct {
	*testfakes.Model
	keys []string
}

func (m *keyModel) Step(ctx context.Context, h []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	m.keys = append(m.keys, app.CacheKeyFrom(ctx))
	return m.Model.Step(ctx, h, tools)
}

func TestModelStepsCarryTheConversationAsCacheKey(t *testing.T) {
	t.Parallel()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := &keyModel{Model: testfakes.NewModel(readNote(10), used("A1", 10), used("A2", 10))}
	tools := testfakes.NewTools(task.ToolSpec{Name: "note.read", InputSchema: json.RawMessage(`{"type":"object"}`)})
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": m},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.read"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{"note.read": policy.TierRead}}, Clock: time.Now,
	})
	ask(t, a, "Q1")
	ask(t, a, "Q2")
	if len(m.keys) != 3 || m.keys[0] != "c1" || m.keys[1] != "c1" || m.keys[2] != "c1" {
		t.Fatalf("cache keys = %v, want the conversation key c1 on every step", m.keys)
	}
}

func TestImagesSurviveTheCapAndReplay(t *testing.T) {
	t.Parallel()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	img := task.Image{MediaType: "image/png", Data: []byte("\x89PNG fake")}
	model := testfakes.NewModel(readNote(10), used("A1", 10), used("A2", 10))
	tools := testfakes.NewTools(task.ToolSpec{Name: "note.read", InputSchema: json.RawMessage(`{"type":"object"}`)})
	tools.Handlers["note.read"] = func(c task.ToolCall) (task.ToolResult, error) {
		return task.ToolResult{CallID: c.ID, Content: strings.Repeat("x", 50<<10), Images: []task.Image{img}}, nil
	}
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.read"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{"note.read": policy.TierRead}}, Clock: time.Now,
	})
	ask(t, a, "Q1")
	ask(t, a, "Q2")
	for i, h := range [][]task.Message{model.Histories[1], model.Histories[2]} {
		var got []task.Image
		for _, m := range h {
			for _, r := range m.Results {
				got = append(got, r.Images...)
			}
		}
		if len(got) != 1 || got[0].MediaType != img.MediaType || string(got[0].Data) != string(img.Data) {
			t.Fatalf("history %d images = %+v, want the one image kept", i+1, got)
		}
	}
}
