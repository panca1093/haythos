package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/app"
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
	if err != nil || len(m.Models) != 3 {
		t.Fatalf("main: %+v err=%v", m, err)
	}
	p, err := LoadPolicy("../../config/policy.yaml")
	if err != nil || p.Tiers["vmhealth.read"] != 0 || p.Version == "" {
		t.Fatalf("policy: %+v err=%v", p, err)
	}
	jobs, err := LoadJobs("../../jobs", nil)
	if err != nil || len(jobs) != 3 || jobs[2].Name != "vm-health" || jobs[2].Collect != "vmhealth.read" || jobs[2].Trigger.Cron == "" {
		t.Fatalf("jobs: %+v err=%v", jobs, err)
	}
	if jobs[0].Name != "brain-consolidate" || !jobs[0].Silent || jobs[0].BrainSource != "auto/consolidate" || jobs[1].Name != "brain-digest" {
		t.Fatalf("brain jobs: %+v err=%v", jobs[:2], err)
	}
}

func TestRejectsUnknownFieldsAndBadValues(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "name: a\ngoal: g\ntools: [t]\nmode: background\nbudget: {tokens: 1, steps: 1}\nbogus: 1\n")
	if _, err := LoadJobs(dir, nil); err == nil {
		t.Error("unknown field accepted")
	}
	dir = t.TempDir()
	write(t, dir, "a.yaml", "name: a\ngoal: g\ntools: [t]\nmode: sideways\nbudget: {tokens: 1, steps: 1}\n")
	if _, err := LoadJobs(dir, nil); err == nil {
		t.Error("bad mode accepted")
	}
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

func TestJobDeliverTo(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "name: a\ngoal: g\ntools: [t]\nmode: background\nbudget: {tokens: 1, steps: 1}\ndeliver_to: ['123', dm]\n")
	jobs, err := LoadJobs(dir, nil)
	if err != nil || len(jobs) != 1 || !reflect.DeepEqual(jobs[0].DeliverTo, []string{"123", "dm"}) {
		t.Fatalf("jobs = %+v, err = %v", jobs, err)
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
		"unknown kind":               "  a: { kind: telepathy, command: x }\n",
		"acp without cmd":            "  a: { kind: acp }\n",
		"relative workdir":           "  a: { kind: acp, command: x, commands_ask: true, workdirs: { w: rel/path } }\n",
		"env without equal":          "  a: { kind: acp, command: x, commands_ask: true, env: [NOEQUALS] }\n",
		"bad guard regexp":           "  a: { kind: acp, command: x, commands_ask: true, guard: [{ name: r, pattern: '(' }] }\n",
		"bad guard action":           "  a: { kind: acp, command: x, commands_ask: true, guard: [{ name: r, pattern: x, action: allow }] }\n",
		"unknown field":              "  a: { kind: acp, command: x, commands_ask: true, shell: yes }\n",
		"acp that never asks":        "  a: { kind: acp, command: x }\n",
		"opencode bash allowed":      "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":\"allow\"}}'] }\n",
		"opencode bash star allowed": "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":{\"*\":\"allow\"}}}'] }\n",
		"opencode config not json":   "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT=nope'] }\n",
		"claudecli env":              "  a: { kind: claudecli, command: x, env: ['A=b'] }\n",
		"claudecli mode":             "  a: { kind: claudecli, command: x, mode: build }\n",
		"codexapp mode":              "  a: { kind: codexapp, command: x, mode: build }\n",
		"alias with dot":             "  a.b: { kind: acp, command: x, commands_ask: true }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", head+body)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestRuntimeJobs(t *testing.T) {
	rts := map[string]Runtime{"oc": {Kind: "acp", Command: "x", Model: "m1", Mode: "build", Workdirs: map[string]string{"scratch": "/work/scratch"}}}
	const head = "name: j\ngoal: do it\ntrigger: { manual: true }\n"
	ok := head + "runtime: { alias: oc, workdir: scratch, model: m2 }\nchecks:\n  - { name: t, argv: [go, test, ./...], timeout: 5m }\n  - { name: v, argv: [go, vet, ./...] }\n"
	jobs, err := LoadJobs(writeJob(t, ok), rts)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, err = %v", jobs, err)
	}
	j := jobs[0]
	if j.Runtime != "oc" || j.Workdir != "/work/scratch" || j.RuntimeModel != "m2" || j.RuntimeMode != "build" {
		t.Errorf("job = %+v, want alias oc, resolved workdir, own model, the runtime's mode", j)
	}
	if len(j.Checks) != 2 || j.Checks[0].Timeout != 5*time.Minute || j.Checks[1].Timeout != app.DefaultCheckTimeout || strings.Join(j.Checks[0].Argv, " ") != "go test ./..." {
		t.Errorf("checks = %+v", j.Checks)
	}

	for name, body := range map[string]string{
		"unknown alias":       head + "runtime: { alias: nope, workdir: scratch }\n",
		"raw path as workdir": head + "runtime: { alias: oc, workdir: /etc }\n",
		"missing workdir":     head + "runtime: { alias: oc }\n",
		"checks without rt":   head + "tools: [t]\nmode: background\nbudget: {tokens: 1, steps: 1}\nchecks: [{ name: t, argv: [x] }]\n",
		"empty argv":          head + "runtime: { alias: oc, workdir: scratch }\nchecks: [{ name: t, argv: [] }]\n",
		"duplicate check":     head + "runtime: { alias: oc, workdir: scratch }\nchecks: [{ name: t, argv: [x] }, { name: t, argv: [y] }]\n",
		"shell string argv":   head + "runtime: { alias: oc, workdir: scratch }\nchecks: [{ name: t, argv: 'go test ./...' }]\n",
		"negative timeout":    head + "runtime: { alias: oc, workdir: scratch }\nchecks: [{ name: t, argv: [x], timeout: -1s }]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadJobs(writeJob(t, body), rts); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func writeJob(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "j.yaml", body)
	return dir
}

func TestExampleRuntimesBlockAndJobLoad(t *testing.T) {
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
	jobs, err := LoadJobs("../../jobs/examples", m.Runtimes)
	if err != nil || len(jobs) != 1 || jobs[0].Runtime != "claude" || jobs[0].Trigger.Cron != "" || !jobs[0].Trigger.Manual {
		t.Fatalf("example job = %+v, err = %v, want a manual job on the claude alias", jobs, err)
	}
}

func TestACPAsksOnlyWhenConfiguredTo(t *testing.T) {
	head := "default_model: sub\nmodels: { sub: { kind: claudesdk } }\nruntimes:\n"
	for name, body := range map[string]string{
		"opencode bash star ask": "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":{\"*\":\"ask\"}}}'] }\n",
		"opencode bash ask":      "  a: { kind: acp, command: x, env: ['OPENCODE_CONFIG_CONTENT={\"permission\":{\"bash\":\"ask\"}}'] }\n",
		"declared by the owner":  "  a: { kind: acp, command: goose, commands_ask: true }\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadMain(write(t, t.TempDir(), "haythos.yaml", head+body)); err != nil {
				t.Error(err)
			}
		})
	}
}
