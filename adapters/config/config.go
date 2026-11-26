package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/panca/haythos/core/policy"
)

type Model struct {
	Kind    string `yaml:"kind"`
	Model   string `yaml:"model"`
	BaseURL string `yaml:"base_url"`
	Secret  string `yaml:"secret"`
	// ContextWindow is the model's context size in tokens; 0 means unknown (the app assumes 200K).
	ContextWindow int `yaml:"context_window"`
}

type MCPServer struct {
	Name    string   `yaml:"name"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	Trusted bool     `yaml:"trusted"`
}

// Runtime is one entry of the runtimes block: how to start a runtime and which folders and guard rules apply to it.
type Runtime struct {
	Kind    string   `yaml:"kind"`
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Model and Mode are defaults; a delegation may override them.
	Model string `yaml:"model"`
	Mode  string `yaml:"mode"`
	// PassEnv names variables copied from the kernel's environment; Env is extra KEY=VALUE.
	PassEnv []string `yaml:"pass_env"`
	Env     []string `yaml:"env"`
	// CommandsAsk is the owner's statement that an acp runtime asks before every shell command; opencode is checked from its env instead.
	CommandsAsk bool `yaml:"commands_ask"`
	// ConfigOptions maps "model" and "mode" to the runtime's config option ids (acp only).
	ConfigOptions map[string]string `yaml:"config_options"`
	// Workdirs maps a name a delegation refers to onto an absolute folder; nothing else is a valid workdir.
	Workdirs map[string]string `yaml:"workdirs"`
	// Guard rules are added to policy.StarterRules unless StarterGuard is false.
	Guard        []policy.GuardRule `yaml:"guard"`
	StarterGuard *bool              `yaml:"starter_guard"`
}

// RuntimeKinds are the kinds buildRuntimes in cmd/haythos can construct.
var RuntimeKinds = []string{"acp", "claudecli", "codexapp"}

func (r Runtime) GuardRules() []policy.GuardRule {
	if r.StarterGuard != nil && !*r.StarterGuard {
		return r.Guard
	}
	return append(policy.StarterRules(), r.Guard...)
}

type Main struct {
	DB           string           `yaml:"db"`
	Vault        string           `yaml:"vault"`
	Soul         string           `yaml:"soul"`
	DefaultModel string           `yaml:"default_model"`
	Models       map[string]Model `yaml:"models"`
	// CompactAt is the share of a model's context window at which a conversation is compacted; 0 means 0.92.
	CompactAt float64 `yaml:"compact_at"`
	ClaudeCLI struct {
		Command string `yaml:"command"`
	} `yaml:"claude_cli"`
	// Deprecated and ignored since the Node sidecar was removed; still decoded so older configs keep loading.
	Sidecar struct {
		Command string   `yaml:"command"`
		Args    []string `yaml:"args"`
		Dir     string   `yaml:"dir"`
	} `yaml:"sidecar"`
	MCP struct {
		Servers []MCPServer `yaml:"servers"`
	} `yaml:"mcp"`
	Discord struct {
		OwnerID string `yaml:"owner_id"`
	} `yaml:"discord"`
	Runtimes map[string]Runtime `yaml:"runtimes"`
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
	if m.ClaudeCLI.Command == "" {
		m.ClaudeCLI.Command = "claude"
	}
	if m.DefaultModel == "" || m.Models[m.DefaultModel].Kind == "" {
		return m, fmt.Errorf("%s: default_model %q is not defined in models", path, m.DefaultModel)
	}
	for name, md := range m.Models {
		switch md.Kind {
		case "anthropic", "openai", "claudesdk", "chatgpt":
		default:
			return m, fmt.Errorf("%s: model %s has unknown kind %q", path, name, md.Kind)
		}
	}
	seen := map[string]bool{}
	for _, sv := range m.MCP.Servers {
		if sv.Name == "" || sv.Command == "" || strings.Contains(sv.Name, ".") {
			return m, fmt.Errorf("%s: mcp server needs a name without dots and a command, got name %q", path, sv.Name)
		}
		if seen[sv.Name] {
			return m, fmt.Errorf("%s: mcp server %q is listed twice", path, sv.Name)
		}
		seen[sv.Name] = true
	}
	for alias, rt := range m.Runtimes {
		if err := validRuntime(alias, &rt, m.ClaudeCLI.Command); err != nil {
			return m, fmt.Errorf("%s: %w", path, err)
		}
		m.Runtimes[alias] = rt
	}
	return m, nil
}

func validRuntime(alias string, rt *Runtime, claudeCommand string) error {
	switch {
	case alias == "" || strings.ContainsAny(alias, ". \t"):
		return fmt.Errorf("runtime alias %q must not be empty or contain dots or spaces", alias)
	case !contains(RuntimeKinds, rt.Kind):
		return fmt.Errorf("runtime %s has unknown kind %q (want one of %s)", alias, rt.Kind, strings.Join(RuntimeKinds, ", "))
	}
	if rt.Command == "" && rt.Kind == "claudecli" {
		rt.Command = claudeCommand
	}
	if rt.Command == "" {
		return fmt.Errorf("runtime %s needs a command", alias)
	}
	for _, kv := range rt.Env {
		if !strings.Contains(kv, "=") {
			return fmt.Errorf("runtime %s: env entry %q must be KEY=VALUE", alias, kv)
		}
	}
	switch {
	case rt.Kind == "claudecli" && len(rt.Env) > 0:
		return fmt.Errorf("runtime %s: claudecli ignores env, use pass_env", alias)
	case rt.Kind != "acp" && rt.Mode != "":
		return fmt.Errorf("runtime %s: %s has no modes, drop mode", alias, rt.Kind)
	}
	if rt.Kind == "acp" && !rt.CommandsAsk && !opencodeAsksForBash(rt.Env) {
		return fmt.Errorf("runtime %s: an acp runtime must be made to ask before shell commands: set OPENCODE_CONFIG_CONTENT with permission.bash ask and permission.execute deny in env, or commands_ask: true for another runtime", alias)
	}
	for name, dir := range rt.Workdirs {
		if name == "" || !filepath.IsAbs(dir) {
			return fmt.Errorf("runtime %s: workdir %q must map to an absolute path, got %q", alias, name, dir)
		}
	}
	if _, err := policy.NewGuard(rt.GuardRules()); err != nil {
		return fmt.Errorf("runtime %s: %w", alias, err)
	}
	return nil
}

func opencodeAsksForBash(env []string) bool {
	const key = "OPENCODE_CONFIG_CONTENT="
	for _, kv := range env {
		raw, ok := strings.CutPrefix(kv, key)
		if !ok {
			continue
		}
		var c struct {
			Permission struct {
				Bash    json.RawMessage `json:"bash"`
				Execute string          `json:"execute"`
			} `json:"permission"`
		}
		// "ask" has no effect on opencode's execute code tool, which reaches the network unprompted (FINDINGS §9).
		if json.Unmarshal([]byte(raw), &c) != nil || c.Permission.Execute != "deny" {
			return false
		}
		var plain string
		var byPattern map[string]string
		if json.Unmarshal(c.Permission.Bash, &plain) == nil {
			return plain == "ask"
		}
		if json.Unmarshal(c.Permission.Bash, &byPattern) != nil || byPattern["*"] != "ask" {
			return false
		}
		for _, action := range byPattern {
			if action == "allow" {
				return false
			}
		}
		return true
	}
	return false
}

func LoadPolicy(path string) (policy.Policy, error) {
	var f struct {
		Version string              `yaml:"version"`
		Tiers   map[string]int      `yaml:"tiers"`
		Secrets map[string][]string `yaml:"secrets"`
		// AutoWhenClean names tier-1 writes that skip approval in an untainted owner task.
		AutoWhenClean []string `yaml:"auto_when_clean"`
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
	for _, tool := range f.AutoWhenClean {
		if p.Tiers[tool] != policy.TierInternalWrite {
			return p, fmt.Errorf("%s: auto_when_clean tool %s must be listed at tier 1", path, tool)
		}
	}
	p.AutoWhenClean = f.AutoWhenClean
	for tool := range f.Secrets {
		if _, ok := p.Tiers[tool]; !ok {
			return p, fmt.Errorf("%s: secrets bound to %s, which has no tier", path, tool)
		}
	}
	return p, nil
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
