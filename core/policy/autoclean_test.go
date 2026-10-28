package policy

import "testing"

func TestEvaluateAutoWhenClean(t *testing.T) {
	p := Policy{
		Version: "v1",
		Tiers: map[string]Tier{
			"brain.remember": TierInternalWrite, "brain.forget": TierInternalWrite,
			"other": TierInternalWrite, "ext": TierExternalWrite, "r": TierRead,
		},
		AutoWhenClean: []string{"brain.remember", "brain.forget", "ext", "r"},
	}
	for _, tc := range []struct {
		name      string
		in        Input
		action    Action
		tier      Tier
		escalated bool
	}{
		{"owner remember runs", Input{Tool: "brain.remember", Write: true, Source: "owner/t1"}, Allow, TierInternalWrite, false},
		{"owner forget runs", Input{Tool: "brain.forget", Write: true, Source: "owner/t1", Target: "42"}, Allow, TierInternalWrite, false},
		{"auto remember runs", Input{Tool: "brain.remember", Write: true, Source: "auto/t1"}, Allow, TierInternalWrite, false},
		{"auto consolidate remember runs", Input{Tool: "brain.remember", Write: true, Source: "auto/consolidate"}, Allow, TierInternalWrite, false},
		{"auto forget of inbox slug runs", Input{Tool: "brain.forget", Write: true, Source: "auto/consolidate", Target: "inbox/x"}, Allow, TierInternalWrite, false},
		{"auto forget of other slug asks", Input{Tool: "brain.forget", Write: true, Source: "auto/consolidate", Target: "entities/x"}, Ask, TierInternalWrite, false},
		{"auto forget without target asks", Input{Tool: "brain.forget", Write: true, Source: "auto/consolidate"}, Ask, TierInternalWrite, false},
		{"owner forget of inbox-less slug still runs", Input{Tool: "brain.forget", Write: true, Source: "owner/t1", Target: "entities/x"}, Allow, TierInternalWrite, false},
		{"no source asks", Input{Tool: "brain.remember", Write: true}, Ask, TierInternalWrite, false},
		{"unknown source asks", Input{Tool: "brain.remember", Write: true, Source: "import/mybook"}, Ask, TierInternalWrite, false},
		{"tainted owner asks and escalates", Input{Tool: "brain.remember", Write: true, Source: "owner/t1", Tainted: true}, Ask, TierHighImpact, true},
		{"tainted auto asks and escalates", Input{Tool: "brain.remember", Write: true, Source: "auto/t1", Tainted: true}, Ask, TierHighImpact, true},
		{"unlisted tier 1 asks", Input{Tool: "other", Write: true, Source: "owner/t1"}, Ask, TierInternalWrite, false},
		{"listed tier 2 still asks", Input{Tool: "ext", Write: true, Source: "owner/t1"}, Ask, TierExternalWrite, false},
		{"listed read stays allow", Input{Tool: "r", Source: "owner/t1"}, Allow, TierRead, false},
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
