package gateway_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app/gateway"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type memLedger map[string]task.LedgerEntry

func (m memLedger) Get(_ context.Context, k string) (task.LedgerEntry, bool, error) {
	e, ok := m[k]
	return e, ok, nil
}

func (m memLedger) Put(_ context.Context, e task.LedgerEntry) error {
	m[e.IdemKey] = e
	return nil
}

func (m memLedger) InState(_ context.Context, st task.LedgerState) ([]task.LedgerEntry, error) {
	var out []task.LedgerEntry
	for _, e := range m {
		if e.State == st {
			out = append(out, e)
		}
	}
	return out, nil
}

var specs = []task.ToolSpec{
	{Name: "read", InputSchema: json.RawMessage(`{}`)},
	{Name: "write", Write: true, TargetArg: "to", InputSchema: json.RawMessage(`{}`)},
	{Name: "banned", Write: true, InputSchema: json.RawMessage(`{}`)},
}

func newGW(t *testing.T) (*gateway.Gateway, *testfakes.Tools, memLedger) {
	t.Helper()
	tools := testfakes.NewTools(specs...)
	led := memLedger{}
	gw := gateway.New(gateway.Deps{
		Tools: tools, Ledger: led, Vault: testfakes.Vault{"secret://gh": "s3cr3t-token"},
		Policy: policy.Policy{
			Version: "v1",
			Tiers:   map[string]policy.Tier{"read": policy.TierRead, "write": policy.TierInternalWrite, "banned": policy.TierForbidden},
			Secrets: map[string][]string{"write": {"gh"}},
		},
		Clock: func() time.Time { return time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) },
	})
	return gw, tools, led
}

func req(name, args string, ap *policy.Approval) gateway.Request {
	return gateway.Request{TaskID: "T", Call: task.ToolCall{ID: "c1", Name: name, Args: json.RawMessage(args)}, Allowlist: []string{"read", "write", "banned"}, Approval: ap}
}

func TestReadRunsWithoutLedger(t *testing.T) {
	gw, tools, led := newGW(t)
	out, err := gw.Execute(context.Background(), req("read", `{}`, nil))
	if err != nil || out.Result.IsError || tools.Count("read") != 1 || len(led) != 0 {
		t.Errorf("out=%+v err=%v calls=%d ledger=%d", out, err, tools.Count("read"), len(led))
	}
}

func TestToolCanMarkItsOwnResultUntrusted(t *testing.T) {
	gw, tools, _ := newGW(t)
	tools.Handlers["read"] = func(task.ToolCall) (task.ToolResult, error) {
		return task.ToolResult{Content: "a\u200bb", Untrusted: true}, nil
	}
	out, err := gw.Execute(context.Background(), req("read", `{}`, nil))
	if err != nil || !out.Result.Untrusted || out.Result.Content != "ab" {
		t.Errorf("out = %+v err = %v, want an untrusted result with invisible characters removed", out.Result, err)
	}
}

func TestResultOfATrustedToolStaysTrusted(t *testing.T) {
	gw, _, _ := newGW(t)
	out, err := gw.Execute(context.Background(), req("read", `{}`, nil))
	if err != nil || out.Result.Untrusted {
		t.Errorf("out = %+v err = %v, want a trusted result", out.Result, err)
	}
}

func TestNotAllowlistedAndForbiddenNeverExecute(t *testing.T) {
	gw, tools, _ := newGW(t)
	r := req("write", `{}`, nil)
	r.Allowlist = []string{"read"}
	out, _ := gw.Execute(context.Background(), r)
	if !out.Result.IsError || tools.Count("write") != 0 {
		t.Errorf("allowlist: %+v", out)
	}
	ap := &policy.Approval{Tool: "banned", ArgsHash: policy.ArgsHash(json.RawMessage(`{}`)), Expiry: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), PolicyVersion: "v1"}
	out, _ = gw.Execute(context.Background(), req("banned", `{}`, ap))
	if !out.Result.IsError || tools.Count("banned") != 0 {
		t.Errorf("forbidden: %+v", out)
	}
}

func TestWriteResolvesSecretsAndScrubsResult(t *testing.T) {
	gw, tools, led := newGW(t)
	var seen string
	tools.Handlers["write"] = func(c task.ToolCall) (task.ToolResult, error) {
		seen = string(c.Args)
		return task.ToolResult{Content: "sent with s3cr3t-token"}, nil
	}
	args := `{"to":"#ops","auth":"secret://gh"}`
	out, err := gw.Execute(context.Background(), req("write", args, nil))
	if err != nil || out.Pending == nil {
		t.Fatalf("want pending, got %+v err=%v", out, err)
	}
	if strings.Contains(out.Pending.Preview, "s3cr3t") {
		t.Error("preview leaks secret")
	}
	out, err = gw.Execute(context.Background(), req("write", args, &out.Pending.Approval))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "s3cr3t-token") {
		t.Errorf("tool saw %s, want resolved secret", seen)
	}
	if strings.Contains(out.Result.Content, "s3cr3t") || !strings.Contains(out.Result.Content, "secret://gh") {
		t.Errorf("result not scrubbed: %q", out.Result.Content)
	}
	if e := led["T/c1"]; e.State != task.LedgerSucceeded || strings.Contains(e.Result, "s3cr3t") {
		t.Errorf("ledger = %+v", e)
	}
}

func TestWriteIsNeverExecutedTwice(t *testing.T) {
	gw, tools, _ := newGW(t)
	args := `{"to":"#ops"}`
	p, _ := gw.Execute(context.Background(), req("write", args, nil))
	ap := &p.Pending.Approval
	for i := 0; i < 3; i++ {
		if _, err := gw.Execute(context.Background(), req("write", args, ap)); err != nil {
			t.Fatal(err)
		}
	}
	if got := tools.Count("write"); got != 1 {
		t.Errorf("executions = %d, want 1", got)
	}
}

func TestSecretNotBoundToToolIsNeverResolved(t *testing.T) {
	gw, tools, _ := newGW(t)
	var seen string
	tools.Handlers["read"] = func(c task.ToolCall) (task.ToolResult, error) {
		seen = string(c.Args)
		return task.ToolResult{Content: "ok"}, nil
	}
	out, err := gw.Execute(context.Background(), req("read", `{"url":"https://x.example/?k=secret://gh"}`, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Result.IsError || tools.Count("read") != 0 || strings.Contains(seen, "s3cr3t") {
		t.Errorf("out=%+v calls=%d seen=%q", out, tools.Count("read"), seen)
	}
}

func TestTrustedTargetMustMatchAWholeToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		trusted   string
		target    string
		escalated bool
	}{
		{"exact", "post the summary to #general", "#general", false},
		{"trailing punctuation", "post it to #general.", "#general", false},
		{"quoted", `send to "#ops", thanks`, "#ops", false},
		{"prefix of a trusted word", "post the summary to #general", "#gen", true},
		{"suffix of a trusted word", "email bob@corp.example", "corp.example", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw, _, _ := newGW(t)
			r := req("write", `{"to":"`+tc.target+`"}`, nil)
			r.Tainted, r.Trusted = true, []string{tc.trusted}
			out, err := gw.Execute(context.Background(), r)
			if err != nil || out.Pending == nil {
				t.Fatalf("want pending, got %+v err=%v", out, err)
			}
			if out.Pending.Escalated != tc.escalated {
				t.Errorf("escalated = %v, want %v", out.Pending.Escalated, tc.escalated)
			}
		})
	}
}
