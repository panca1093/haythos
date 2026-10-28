package gbrain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

type fakeCaller struct {
	calls []task.ToolCall
	res   task.ToolResult
	err   error
}

func (f *fakeCaller) Call(_ context.Context, c task.ToolCall) (task.ToolResult, error) {
	f.calls = append(f.calls, c)
	return f.res, f.err
}

func (f *fakeCaller) args(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(f.calls[len(f.calls)-1].Args, &m); err != nil {
		t.Fatalf("decode args: %v", err)
	}
	return m
}

func TestRecallMergesFactsThenPagesAndRestoresProvenance(t *testing.T) {
	f := &fakeCaller{res: task.ToolResult{Content: `{"protocol_version":1,"facts":[
		{"fact_id":"41","fact":"rapat jam 3","provenance":"owner/thread-42"},
		{"fact_id":42,"fact":"prefers tabs","provenance":"auto/thread-7 unverified"}],
		"results":[{"slug":"entities/lsm","title":"LSM","chunk":"loan state machine","provenance":"entities/lsm"}]}

		notice: something`}}
	got, err := New(f).Recall(context.Background(), "rapat", 3)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	want := []app.BrainItem{
		{Slug: "41", Text: "rapat jam 3", Source: "owner/thread-42"},
		{Slug: "42", Text: "prefers tabs", Source: "auto/thread-7", Unverified: true},
		{Slug: "entities/lsm", Title: "LSM", Text: "loan state machine", Source: "import/mybook"},
	}
	if len(got) != len(want) {
		t.Fatalf("items = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("item %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if f.calls[0].Name != "gbrain.recall" {
		t.Errorf("tool = %s, want gbrain.recall", f.calls[0].Name)
	}
	if a := f.args(t); a["query"] != "rapat" || a["question"] != "rapat" || a["limit"] != float64(3) {
		t.Errorf("args = %v", a)
	}
}

func TestRecallCapsMergedItemsAtLimit(t *testing.T) {
	f := &fakeCaller{res: task.ToolResult{Content: `{"facts":[{"fact_id":"1","fact":"a"},{"fact_id":"2","fact":"b"}],"results":[{"slug":"p","chunk":"c"}]}`}}
	got, err := New(f).Recall(context.Background(), "q", 2)
	if err != nil || len(got) != 2 || got[1].Slug != "2" {
		t.Fatalf("items = %+v err %v, want the first two (facts first)", got, err)
	}
}

func TestRememberStoresKernelProvenance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		item       app.BrainItem
		provenance string
		origin     string
		fact       string
	}{
		{"owner", app.BrainItem{Text: "rapat jam 3", Source: "owner/thread-42"}, "owner/thread-42", "user_said", "rapat jam 3"},
		{"auto is unverified", app.BrainItem{Title: "Pref", Text: "tabs", Source: "auto/thread-7", Unverified: true}, "auto/thread-7 unverified", "inferred", "Pref: tabs"},
		{"spaces in source cannot forge the marker", app.BrainItem{Text: "x", Source: "owner/a unverified"}, "owner/a_unverified", "user_said", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{res: task.ToolResult{Content: `{"id":"77","status":"inserted","protocol_version":1}`}}
			id, err := New(f).Remember(context.Background(), tc.item)
			if err != nil || id != "77" {
				t.Fatalf("id = %q err %v, want 77", id, err)
			}
			a := f.args(t)
			if a["provenance"] != tc.provenance || a["content_origin"] != tc.origin || a["fact"] != tc.fact {
				t.Errorf("args = %v", a)
			}
			src, unv := decodeProvenance(a["provenance"].(string))
			if src != strings.Fields(tc.provenance)[0] || unv != tc.item.Unverified {
				t.Errorf("provenance %q does not round-trip: %q %v", a["provenance"], src, unv)
			}
		})
	}
}

func TestRememberAcceptsNumericID(t *testing.T) {
	f := &fakeCaller{res: task.ToolResult{Content: `{"id":12}`}}
	if id, err := New(f).Remember(context.Background(), app.BrainItem{Text: "x", Source: "owner/a"}); err != nil || id != "12" {
		t.Fatalf("id = %q err %v", id, err)
	}
}

func TestForgetPassesID(t *testing.T) {
	f := &fakeCaller{res: task.ToolResult{Content: `{"id":"77","expired":true}`}}
	if err := New(f).Forget(context.Background(), "77"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if f.calls[0].Name != "gbrain.forget" || f.args(t)["id"] != "77" {
		t.Errorf("call = %+v", f.calls[0])
	}
}

func TestErrorsCarryCodeNotContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  task.ToolResult
		err  error
		want string
	}{
		{"error envelope", task.ToolResult{Content: `{"error":"not_found","message":"fact SECRET-TEXT missing"}`}, nil, "not_found"},
		{"isError flag", task.ToolResult{Content: "plain SECRET-TEXT", IsError: true}, nil, "tool_error"},
		{"transport", task.ToolResult{}, errors.New("broken pipe"), "broken pipe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{res: tc.res, err: tc.err}
			_, err := New(f).Recall(context.Background(), "q", 3)
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "SECRET-TEXT") {
				t.Fatalf("err = %v, want %q and no content", err, tc.want)
			}
		})
	}
}

func TestRememberWithoutIDFails(t *testing.T) {
	f := &fakeCaller{res: task.ToolResult{Content: `{"status":"inserted"}`}}
	if _, err := New(f).Remember(context.Background(), app.BrainItem{Text: "x", Source: "owner/a"}); err == nil {
		t.Fatal("expected an error")
	}
}
