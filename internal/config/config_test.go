package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
	m, err := LoadMain("../../config/haythos.yaml")
	if err != nil || len(m.Models) != 3 {
		t.Fatalf("main: %+v err=%v", m, err)
	}
	p, err := LoadPolicy("../../config/policy.yaml")
	if err != nil || p.Tiers["vmhealth.read"] != 0 || p.Version == "" {
		t.Fatalf("policy: %+v err=%v", p, err)
	}
	jobs, err := LoadJobs("../../jobs")
	if err != nil || len(jobs) != 1 || jobs[0].Name != "vm-health" || jobs[0].Collect != "vmhealth.read" || jobs[0].Trigger.Cron == "" {
		t.Fatalf("jobs: %+v err=%v", jobs, err)
	}
}

func TestRejectsUnknownFieldsAndBadValues(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.yaml", "name: a\ngoal: g\ntools: [t]\nmode: background\nbudget: {tokens: 1, steps: 1}\nbogus: 1\n")
	if _, err := LoadJobs(dir); err == nil {
		t.Error("unknown field accepted")
	}
	dir = t.TempDir()
	write(t, dir, "a.yaml", "name: a\ngoal: g\ntools: [t]\nmode: sideways\nbudget: {tokens: 1, steps: 1}\n")
	if _, err := LoadJobs(dir); err == nil {
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
  channel_id: "42"
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
