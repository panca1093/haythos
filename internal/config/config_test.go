package config

import (
	"os"
	"path/filepath"
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
}
