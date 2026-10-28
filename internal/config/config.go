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
		OwnerID string `yaml:"owner_id"`
		// Deprecated and ignored; still decoded so configs written before it was removed keep loading.
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
		Version string              `yaml:"version"`
		Tiers   map[string]int      `yaml:"tiers"`
		Secrets map[string][]string `yaml:"secrets"`
	}
	if err := decode(path, &f); err != nil {
		return policy.Policy{}, err
	}
	p := policy.Policy{Version: f.Version, Tiers: map[string]policy.Tier{}, Secrets: f.Secrets}
	for tool, t := range f.Tiers {
		if t < int(policy.TierRead) || t > int(policy.TierForbidden) {
			return p, fmt.Errorf("%s: tool %s has tier %d, want 0-4", path, tool, t)
		}
		p.Tiers[tool] = policy.Tier(t)
	}
	for tool := range f.Secrets {
		if _, ok := p.Tiers[tool]; !ok {
			return p, fmt.Errorf("%s: secrets bound to %s, which has no tier", path, tool)
		}
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
	DeliverTo []string    `yaml:"deliver_to"`
	Silent    bool        `yaml:"silent"`
	// BrainSource is the provenance the kernel stamps on brain writes from this job.
	BrainSource string `yaml:"brain_source"`
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
			Budget: f.Budget, Trigger: f.Trigger, Collect: f.Collect, NormalMsg: f.NormalMsg, DeliverTo: f.DeliverTo,
			Silent: f.Silent, BrainSource: f.BrainSource,
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

// SaveModelChoice sets default_model and models.<name>.model in the YAML at path, keeping comments and every other key.
func SaveModelChoice(path, name, model string) error {
	if name == "" || model == "" {
		return fmt.Errorf("save model choice: name and model are required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("%s: top level must be a mapping", path)
	}
	root := doc.Content[0]
	entry := mapValue(mapValue(root, "models"), name)
	if entry == nil || entry.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: model %q is not defined in models", path, name)
	}
	setScalar(root, "default_model", name)
	setScalar(entry, "model", model)

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	return writeAtomic(path, out.Bytes(), fi.Mode().Perm())
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setScalar(m *yaml.Node, key, value string) {
	if v := mapValue(m, key); v != nil {
		v.Kind, v.Tag, v.Value, v.Style = yaml.ScalarNode, "!!str", value, 0
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp for %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
