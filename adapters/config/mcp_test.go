package config

import "testing"

const mcpBase = "default_model: m\nmodels: {m: {kind: claudesdk}}\n"

func TestMCPServers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		extra   string
		wantErr bool
	}{
		{"none", "", false},
		{"gbrain trusted", "mcp:\n  servers:\n    - {name: gbrain, command: gbrain, args: [serve, --surface, verbs], trusted: true}\n", false},
		{"missing command", "mcp:\n  servers:\n    - {name: x}\n", true},
		{"dotted name", "mcp:\n  servers:\n    - {name: a.b, command: c}\n", true},
		{"duplicate name", "mcp:\n  servers:\n    - {name: a, command: c}\n    - {name: a, command: d}\n", true},
		{"unknown field", "mcp:\n  servers:\n    - {name: a, command: c, bogus: 1}\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := LoadMain(write(t, t.TempDir(), "c.yaml", mcpBase+tc.extra))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.name == "gbrain trusted" && (len(m.MCP.Servers) != 1 || !m.MCP.Servers[0].Trusted || len(m.MCP.Servers[0].Args) != 3) {
				t.Fatalf("servers = %+v", m.MCP.Servers)
			}
		})
	}
}

func TestPolicyAutoWhenClean(t *testing.T) {
	d := t.TempDir()
	pol, err := LoadPolicy(write(t, d, "ok.yaml", "version: '1'\ntiers: {brain.remember: 1}\nauto_when_clean: [brain.remember]\n"))
	if err != nil || len(pol.AutoWhenClean) != 1 || pol.AutoWhenClean[0] != "brain.remember" {
		t.Fatalf("policy = %+v err %v", pol, err)
	}
	for name, body := range map[string]string{
		"unlisted tool": "version: '1'\ntiers: {a: 1}\nauto_when_clean: [b]\n",
		"tier 2 tool":   "version: '1'\ntiers: {a: 2}\nauto_when_clean: [a]\n",
		"tier 0 tool":   "version: '1'\ntiers: {a: 0}\nauto_when_clean: [a]\n",
	} {
		if _, err := LoadPolicy(write(t, d, "bad.yaml", body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestShippedPolicyBrainTiers(t *testing.T) {
	p, err := LoadPolicy("../../config/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p.Tiers["brain.recall"] != 0 || p.Tiers["brain.remember"] != 1 || p.Tiers["brain.forget"] != 1 {
		t.Fatalf("tiers = %v", p.Tiers)
	}
	if len(p.AutoWhenClean) != 2 || p.AutoWhenClean[0] != "brain.remember" || p.AutoWhenClean[1] != "brain.forget" {
		t.Fatalf("auto_when_clean = %v", p.AutoWhenClean)
	}
}
