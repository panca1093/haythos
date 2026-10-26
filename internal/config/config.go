package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type Model struct {
	Kind    string `yaml:"kind"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	Secret  string `yaml:"secret"`
}

type Main struct {
	DB           string           `yaml:"db"`
	Vault        string           `yaml:"vault"`
	DefaultModel string           `yaml:"default_model"`
	Models       map[string]Model `yaml:"models"`
	Sidecar      struct {
		Command string   `yaml:"command"`
		Args    []string `yaml:"args"`
		Dir     string   `yaml:"dir"`
	} `yaml:"sidecar"`
	Discord struct {
		OwnerID   string `yaml:"owner_id"`
		ChannelID string `yaml:"channel_id"`
	} `yaml:"discord"`
}

func decode(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func LoadMain(path string) (Main, error) {
	var m Main
	if err := decode(path, &m); err != nil {
		return m, err
	}
	if m.DefaultModel == "" || m.Models[m.DefaultModel].Kind == "" {
		return m, fmt.Errorf("%s: default_model %q is not defined in models", path, m.DefaultModel)
	}
	for name, md := range m.Models {
		switch md.Kind {
		case "anthropic", "openai", "claudesdk":
		default:
			return m, fmt.Errorf("%s: model %s has unknown kind %q", path, name, md.Kind)
		}
	}
	return m, nil
}

func LoadPolicy(path string) (policy.Policy, error) {
	var f struct {
		Version string         `yaml:"version"`
		Tiers   map[string]int `yaml:"tiers"`
	}
	if err := decode(path, &f); err != nil {
		return policy.Policy{}, err
	}
	p := policy.Policy{Version: f.Version, Tiers: map[string]policy.Tier{}}
	for tool, t := range f.Tiers {
		if t < int(policy.TierRead) || t > int(policy.TierForbidden) {
			return p, fmt.Errorf("%s: tool %s has tier %d, want 0-4", path, tool, t)
		}
		p.Tiers[tool] = policy.Tier(t)
	}
	return p, nil
}

type jobFile struct {
	Name      string      `yaml:"name"`
	Goal      string      `yaml:"goal"`
	Model     string      `yaml:"model"`
	Tools     []string    `yaml:"tools"`
	Mode      string      `yaml:"mode"`
	Trigger   app.Trigger `yaml:"trigger"`
	Budget    task.Budget `yaml:"budget"`
	Collect   string      `yaml:"collect"`
	NormalMsg string      `yaml:"normal_msg"`
	Contract  string      `yaml:"contract"`
}

// LoadJobs reads every *.yaml in dir.
func LoadJobs(dir string) ([]app.Job, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	var jobs []app.Job
	for _, p := range paths {
		var f jobFile
		if err := decode(p, &f); err != nil {
			return nil, err
		}
		if f.Name == "" || f.Goal == "" || len(f.Tools) == 0 {
			return nil, fmt.Errorf("%s: name, goal and tools are required", p)
		}
		if f.Mode != "background" && f.Mode != "foreground" {
			return nil, fmt.Errorf("%s: mode must be background or foreground, got %q", p, f.Mode)
		}
		if f.Budget.Tokens == 0 || f.Budget.Steps == 0 {
			return nil, fmt.Errorf("%s: budget tokens and steps are required", p)
		}
		if f.Collect != "" && !contains(f.Tools, f.Collect) {
			return nil, fmt.Errorf("%s: collect tool %s must be in tools", p, f.Collect)
		}
		jobs = append(jobs, app.Job{
			Name: f.Name, Goal: strings.TrimSpace(f.Goal), Model: f.Model, Tools: f.Tools,
			Budget: f.Budget, Trigger: f.Trigger, Collect: f.Collect, NormalMsg: f.NormalMsg,
		})
	}
	return jobs, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
