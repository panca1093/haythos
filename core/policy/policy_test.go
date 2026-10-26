package policy

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	p := Policy{Version: "v1", Tiers: map[string]Tier{"r": TierRead, "w1": TierInternalWrite, "w2": TierExternalWrite, "x": TierForbidden, "badread": TierRead}}
	for _, tc := range []struct {
		name      string
		in        Input
		action    Action
		tier      Tier
		escalated bool
	}{
		{"read auto", Input{Tool: "r"}, Allow, TierRead, false},
		{"read stays auto when tainted", Input{Tool: "r", Tainted: true}, Allow, TierRead, false},
		{"tier1 write asks", Input{Tool: "w1", Write: true}, Ask, TierInternalWrite, false},
		{"tier2 write asks", Input{Tool: "w2", Write: true}, Ask, TierExternalWrite, false},
		{"forbidden denied", Input{Tool: "x", Write: true}, Deny, TierForbidden, false},
		{"unknown denied", Input{Tool: "nope"}, Deny, TierForbidden, false},
		{"write mislabeled as read is raised", Input{Tool: "badread", Write: true}, Ask, TierInternalWrite, false},
		{"tainted write to untrusted target escalates", Input{Tool: "w1", Write: true, Tainted: true}, Ask, TierHighImpact, true},
		{"tainted write to trusted target keeps tier", Input{Tool: "w1", Write: true, Tainted: true, TargetTrusted: true}, Ask, TierInternalWrite, false},
		{"untainted write keeps tier", Input{Tool: "w1", Write: true, TargetTrusted: false}, Ask, TierInternalWrite, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := p.Evaluate(tc.in)
			if d.Action != tc.action || d.Tier != tc.tier || d.Escalated != tc.escalated {
				t.Errorf("got %+v, want action=%s tier=%d escalated=%v", d, tc.action, tc.tier, tc.escalated)
			}
		})
	}
}

func TestArgsHashIgnoresKeyOrder(t *testing.T) {
	a := ArgsHash(json.RawMessage(`{"a":1,"b":[1,2]}`))
	b := ArgsHash(json.RawMessage(`{ "b": [1,2], "a": 1 }`))
	c := ArgsHash(json.RawMessage(`{"a":2,"b":[1,2]}`))
	if a != b || a == c {
		t.Errorf("a=%s b=%s c=%s", a, b, c)
	}
}

func TestValidateApproval(t *testing.T) {
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	ok := Approval{Tool: "t", Target: "#x", ArgsHash: "h", Expiry: now.Add(time.Minute), PolicyVersion: "v1"}
	for _, tc := range []struct {
		name    string
		mutate  func(*Approval)
		tool    string
		target  string
		hash    string
		version string
		now     time.Time
		wantErr bool
	}{
		{"valid", func(*Approval) {}, "t", "#x", "h", "v1", now, false},
		{"tool", func(*Approval) {}, "u", "#x", "h", "v1", now, true},
		{"target", func(*Approval) {}, "t", "#y", "h", "v1", now, true},
		{"hash", func(*Approval) {}, "t", "#x", "g", "v1", now, true},
		{"policy version", func(*Approval) {}, "t", "#x", "h", "v2", now, true},
		{"expired", func(*Approval) {}, "t", "#x", "h", "v1", now.Add(time.Minute), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := ok
			tc.mutate(&a)
			err := ValidateApproval(a, tc.tool, tc.target, tc.hash, tc.now, tc.version)
			if (err != nil) != tc.wantErr || (err != nil && !errors.Is(err, ErrApprovalVoid)) {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
