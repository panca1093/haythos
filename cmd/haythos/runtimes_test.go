package main

import (
	"testing"

	"github.com/panca/haythos/adapters/config"
	"github.com/panca/haythos/adapters/runtime/acp"
	"github.com/panca/haythos/adapters/runtime/claudecli"
	"github.com/panca/haythos/core/policy"
)

func TestBuildRuntimes(t *testing.T) {
	cfg := config.Main{Runtimes: map[string]config.Runtime{
		"oc": {Kind: "acp", Command: "opencode", Args: []string{"acp"}},
		"cc": {Kind: "claudecli", Command: "claude", Model: "haiku", Guard: []policy.GuardRule{{Name: "no-curl", Kinds: []string{policy.KindExecute}, Command: true, Pattern: `^curl\b`, Action: policy.Deny}}},
	}}
	rts, guards, err := buildRuntimes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rts["oc"].(*acp.Runtime); !ok || rts["oc"].Name() != "oc" {
		t.Errorf("oc = %T named %q, want an acp runtime named after its alias", rts["oc"], rts["oc"].Name())
	}
	if _, ok := rts["cc"].(*claudecli.Runtime); !ok {
		t.Errorf("cc = %T, want a claudecli runtime", rts["cc"])
	}
	req := func(text string) policy.GuardRequest {
		return policy.GuardRequest{Kind: policy.KindExecute, Text: text}
	}
	for _, tc := range []struct {
		alias, text string
		want        policy.Action
	}{
		{"oc", "rm -rf x", policy.Ask},
		{"oc", "curl example.test", policy.Allow},
		{"cc", "rm -rf x", policy.Ask},
		{"cc", "curl example.test", policy.Deny},
	} {
		if got := guards[tc.alias].Verdict(req(tc.text)).Action; got != tc.want {
			t.Errorf("%s guard on %q = %s, want %s", tc.alias, tc.text, got, tc.want)
		}
	}
}

func TestBuildRuntimesRejectsUnknownKind(t *testing.T) {
	if _, _, err := buildRuntimes(config.Main{Runtimes: map[string]config.Runtime{"x": {Kind: "telepathy", Command: "x"}}}); err == nil {
		t.Error("an unknown kind was built")
	}
}
