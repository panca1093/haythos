package gateway_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app/gateway"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func TestUntrustedResultsLoseInvisibleCharacters(t *testing.T) {
	for _, tc := range []struct {
		name      string
		untrusted bool
		want      string
	}{{"untrusted tool is cleaned", true, "ab"}, {"trusted tool is left alone", false, "a\u200bb"}} {
		t.Run(tc.name, func(t *testing.T) {
			tools := testfakes.NewTools(task.ToolSpec{Name: "r", Untrusted: tc.untrusted, InputSchema: json.RawMessage(`{}`)})
			tools.Handlers["r"] = func(task.ToolCall) (task.ToolResult, error) { return task.ToolResult{Content: "a\u200bb"}, nil }
			gw := gateway.New(gateway.Deps{Tools: tools, Ledger: memLedger{}, Vault: testfakes.Vault{},
				Policy: policy.Policy{Tiers: map[string]policy.Tier{"r": policy.TierRead}}})
			out, err := gw.Execute(context.Background(), gateway.Request{TaskID: "T", Call: task.ToolCall{ID: "c", Name: "r", Args: json.RawMessage(`{}`)}, Allowlist: []string{"r"}})
			if err != nil || out.Result.Content != tc.want {
				t.Fatalf("content = %q err %v, want %q", out.Result.Content, err, tc.want)
			}
		})
	}
}

func TestBrainSourceLetsAutoListedWriteRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  string
		tainted bool
		pending bool
		runs    int
	}{
		{"owner and clean runs", "owner/t", false, false, 1},
		{"no source asks", "", false, true, 0},
		{"tainted asks", "owner/t", true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := testfakes.NewTools(task.ToolSpec{Name: "w", Write: true, InputSchema: json.RawMessage(`{}`)})
			gw := gateway.New(gateway.Deps{Tools: tools, Ledger: memLedger{}, Vault: testfakes.Vault{},
				Clock:  func() time.Time { return time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) },
				Policy: policy.Policy{Version: "v1", Tiers: map[string]policy.Tier{"w": policy.TierInternalWrite}, AutoWhenClean: []string{"w"}}})
			out, err := gw.Execute(context.Background(), gateway.Request{
				TaskID: "T", Call: task.ToolCall{ID: "c", Name: "w", Args: json.RawMessage(`{}`)}, Allowlist: []string{"w"},
				BrainSource: tc.source, Tainted: tc.tainted,
			})
			if err != nil || (out.Pending != nil) != tc.pending || tools.Count("w") != tc.runs {
				t.Fatalf("pending=%v runs=%d err=%v, want pending=%v runs=%d", out.Pending != nil, tools.Count("w"), err, tc.pending, tc.runs)
			}
		})
	}
}
