package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/panca/haythos/core/policy"
)

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestShippedConfigLoads(t *testing.T) {
	m, err := LoadMain("../../config/haythos.example.yaml")
	if err != nil || len(m.Models) != 3 || m.Soul != "config/SOUL.md" {
		t.Fatalf("main: %+v err=%v", m, err)
	}
	p, err := LoadPolicy("../../config/policy.yaml")
	if err != nil || p.Version == "" {
		t.Fatalf("policy: %+v err=%v", p, err)
	}
	if _, ok := p.Tiers["vmhealth.read"]; ok {
		t.Errorf("vmhealth.read should not be a policy tool")
	}
	if _, ok := p.Tiers["self.about"]; ok {
		t.Errorf("self.about should not be a policy tool")
	}
}

func TestRejectsUnknownFieldsAndBadValues(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "p.yaml", "version: '1'\ntiers: {x: 9}\n")
	if _, err := LoadPolicy(p); err == nil {
		t.Error("tier 9 accepted")
	}
	p = write(t, dir, "p.yaml", "version: '1'\ntiers: {x: 1}\nsecrets: {y: [gh]}\n")
	if _, err := LoadPolicy(p); err == nil {
		t.Error("secret bound to a tool without a tier accepted")
	}
}

func TestPolicySecrets(t *testing.T) {
	p := write(t, t.TempDir(), "p.yaml", "version: '1'\ntiers: {x: 1}\nsecrets: {x: [gh]}\n")
	pol, err := LoadPolicy(p)
	if err != nil || len(pol.Secrets["x"]) != 1 || pol.Secrets["x"][0] != "gh" {
		t.Fatalf("policy: %+v err=%v", pol, err)
	}
}

const savableConfig = `# top comment
db: data/haythos.db
vault: data/vault.enc
default_model: claude-api # trailing note

models:
  # api key route
  claude-api: { kind: anthropic, model: claude-sonnet-5-5, secret: anthropic }
  claude-sub: { kind: claudesdk }
  block:
    kind: openai
    model: gpt-5
    base_url: https://example.test/v1
    secret: openai

# claudesdk starts this child process.
sidecar:
  command: node
  args: [src/index.ts]
  dir: sidecar/claude-sdk

discord:
  owner_id: "100000000000000001"
`

func TestSaveModelChoice(t *testing.T) {
	for _, tc := range []struct {
		name, model, value string
	}{
		{"adds model to a flow mapping", "claude-sub", "opus"},
		{"replaces an existing model", "claude-api", "claude-haiku-4-5-20251001"},
		{"edits a block mapping", "block", "gpt-6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, t.TempDir(), "haythos.yaml", savableConfig)
			before, err := LoadMain(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := SaveModelChoice(path, tc.model, tc.value); err != nil {
				t.Fatal(err)
			}
			after, err := LoadMain(path)
			if err != nil {
				t.Fatal(err)
			}
			want := before
			want.DefaultModel = tc.model
			want.Models = map[string]Model{}
			for k, v := range before.Models {
				want.Models[k] = v
			}
			m := want.Models[tc.model]
			m.Model = tc.value
			want.Models[tc.model] = m
			if !reflect.DeepEqual(after, want) {
				t.Errorf("config = %+v, want %+v", after, want)
			}
			raw, _ := os.ReadFile(path)
			for _, c := range []string{"# top comment", "# trailing note", "# api key route", "# claudesdk starts this child process.", `owner_id: "100000000000000001"`} {
				if !strings.Contains(string(raw), c) {
					t.Errorf("lost %q in:\n%s", c, raw)
				}
			}
		})
	}
}

func TestSaveModelChoiceRejectsUnknownModelAndLeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "haythos.yaml", savableConfig)
	if err := SaveModelChoice(path, "nope", "opus"); err == nil {
		t.Fatal("want error for undefined model name")
	}
	if err := SaveModelChoice(path, "claude-sub", ""); err == nil {
		t.Fatal("want error for empty model value")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != savableConfig {
		t.Errorf("file changed:\n%s", raw)
	}
}

func TestSaveModelChoiceIsAtomicAndKeepsMode(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, "haythos.yaml", savableConfig)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := SaveModelChoice(path, "claude-sub", "opus"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
}

func TestSaveModelChoiceAddsMissingDefaultModelKey(t *testing.T) {
	path := write(t, t.TempDir(), "haythos.yaml", "models:\n  claude-sub: { kind: claudesdk }\n")
	if err := SaveModelChoice(path, "claude-sub", "sonnet"); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMain(path)
	if err != nil || m.DefaultModel != "claude-sub" || m.Models["claude-sub"].Model != "sonnet" {
		t.Errorf("cfg=%+v err=%v", m, err)
	}
}

func TestChatGPTKindLoads(t *testing.T) {
	path := write(t, t.TempDir(), "haythos.yaml", "default_model: gpt-sub\nmodels:\n  gpt-sub: { kind: chatgpt, model: gpt-x }\n")
	m, err := LoadMain(path)
	if err != nil || m.Models["gpt-sub"].Kind != "chatgpt" || m.Models["gpt-sub"].Model != "gpt-x" {
		t.Errorf("cfg=%+v err=%v", m, err)
	}
}

func TestClaudeCLIConfig(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"explicit command", "claude_cli: { command: /opt/claude }\n", "/opt/claude"},
		{"default command", "", "claude"},
		{"deprecated sidecar block still loads", "sidecar: { command: node, args: [src/index.ts], dir: sidecar/claude-sdk }\n", "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", "default_model: sub\nmodels: { sub: { kind: claudesdk } }\n"+tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if m.ClaudeCLI.Command != tc.want {
				t.Errorf("command = %q, want %q", m.ClaudeCLI.Command, tc.want)
			}
		})
	}
}

const runtimesYAML = `default_model: sub
models: { sub: { kind: claudesdk } }
runtimes:
  oc:
    kind: acp
    command: opencode
    commands_ask: true
    args: [acp]
    model: m1
    mode: build
    pass_env: [FOO_KEY]
    env: ["A=b"]
    config_options: { model: model_id }
    workdirs: { scratch: /work/scratch }
  cc:
    kind: claudecli
    workdirs: { app: /work/app }
    starter_guard: false
    guard:
      - { name: no-curl, kinds: [execute], command: true, pattern: '^curl\b', action: deny }
`

func TestRuntimesLoad(t *testing.T) {
	m, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", runtimesYAML))
	if err != nil {
		t.Fatal(err)
	}
	oc, cc := m.Runtimes["oc"], m.Runtimes["cc"]
	if oc.Kind != "acp" || oc.Command != "opencode" || oc.Model != "m1" || oc.Mode != "build" || oc.Workdirs["scratch"] != "/work/scratch" || oc.ConfigOptions["model"] != "model_id" {
		t.Errorf("oc = %+v", oc)
	}
	if cc.Command != "claude" {
		t.Errorf("claudecli command = %q, want the claude_cli default", cc.Command)
	}
	if got := len(oc.GuardRules()); got != len(policy.StarterRules()) {
		t.Errorf("default guard has %d rules, want the starter list (%d)", got, len(policy.StarterRules()))
	}
	if got := cc.GuardRules(); len(got) != 1 || got[0].Name != "no-curl" {
		t.Errorf("starter_guard false: rules = %+v, want only the owner's", got)
	}
}

func TestRuntimesAddToStarterGuard(t *testing.T) {
	body := "default_model: sub\nmodels: { sub: { kind: claudesdk } }\nruntimes:\n  oc:\n    kind: acp\n    command: x\n    commands_ask: true\n    guard: [{ name: extra, pattern: zzz }]\n"
	m, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", body))
	if err != nil {
		t.Fatal(err)
	}
	rules := m.Runtimes["oc"].GuardRules()
	if len(rules) != len(policy.StarterRules())+1 || rules[len(rules)-1].Name != "extra" {
		t.Errorf("rules = %d, want the starter list plus extra", len(rules))
	}
}

func TestRuntimesRejectBadEntries(t *testing.T) {
	head := "default_model: sub\nmodels: { sub: { kind: claudesdk } }\nruntimes:\n"
	for name, body := range map[string]string{
		"unknown kind":                   "  a: { kind: telepathy, command: x }\n",
		"acp without cmd":                "  a: { kind: acp }\n",
		"relative workdir":               "  a: { kind: acp, command: x, commands_ask: true, workdirs: { w: rel/path } }\n",
		"env without equal":              "  a: { kind: acp, command: x, commands_ask: true, env: [NOEQUALS] }\n",
		"bad guard regexp":               "  a: { kind: acp, command: x, commands_ask: true, guard: [{ name: r, pattern: '(' }] }\n",
		"bad guard action":               "  a: { kind: acp, command: x, commands_ask: true, guard: [{ name: r, pattern: x, action: allow }] }\n",
		"unknown field":                  "  a: { kind: acp, command: x, commands_ask: true, shell: yes }\n",
		"acp that never asks":            "  a: { kind: acp, command: x }\n",
		"opencode bash allowed":          "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":\"allow\"}}'] }\n",
		"opencode bash star allowed":     "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":{\"*\":\"allow\"}}}'] }\n",
		"opencode other pattern allowed": "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":{\"*\":\"ask\",\"git *\":\"allow\"}}}'] }\n",
		"opencode execute not denied":    "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":{\"*\":\"ask\"}}}'] }\n",
		"opencode execute asks":          "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"execute\":\"ask\",\"bash\":{\"*\":\"ask\"}}}'] }\n",
		"opencode config not json":       "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT=nope'] }\n",
		"claudecli env":                  "  a: { kind: claudecli, command: x, env: ['A=b'] }\n",
		"claudecli mode":                 "  a: { kind: claudecli, command: x, mode: build }\n",
		"codexapp mode":                  "  a: { kind: codexapp, command: x, mode: build }\n",
		"alias with dot":                 "  a.b: { kind: acp, command: x, commands_ask: true }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", head+body)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestOpencodeBashPatternsMayAskOrDenyButNotAllow(t *testing.T) {
	head := "default_model: sub\nmodels: { sub: { kind: claudesdk } }\nruntimes:\n"
	body := "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"execute\":\"deny\",\"bash\":{\"*\":\"ask\",\"rm *\":\"deny\",\"ls\":\"ask\"}}}'] }\n"
	if _, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", head+body)); err != nil {
		t.Errorf("rejected ask and deny patterns: %v", err)
	}
}

func TestExampleRuntimesBlockLoads(t *testing.T) {
	b, err := os.ReadFile("../../config/haythos.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var block []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		if line == "# runtimes:" {
			in = true
		}
		if in {
			block = append(block, strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "))
		}
	}
	if len(block) == 0 {
		t.Fatal("no commented runtimes block in the example config")
	}
	path := write(t, t.TempDir(), "haythos.yaml", string(b)+"\n"+strings.Join(block, "\n")+"\n")
	m, err := LoadMain(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Runtimes["opencode"].Kind != "acp" || m.Runtimes["claude"].Kind != "claudecli" {
		t.Fatalf("runtimes = %+v", m.Runtimes)
	}
}

func TestACPAsksOnlyWhenConfiguredTo(t *testing.T) {
	head := "default_model: sub\nmodels: { sub: { kind: claudesdk } }\nruntimes:\n"
	for name, body := range map[string]string{
		"opencode bash star ask": "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"execute\":\"deny\",\"bash\":{\"*\":\"ask\"}}}'] }\n",
		"opencode bash ask":      "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"execute\":\"deny\",\"bash\":\"ask\"}}'] }\n",
		"declared by the owner":  "  a: { kind: acp, command: goose, commands_ask: true }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", head+body)); err != nil {
				t.Error(err)
			}
		})
	}
}
