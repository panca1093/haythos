package policy

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	KindExecute = "execute"
	KindRead    = "read"
	KindEdit    = "edit"
	KindDelete  = "delete"
)

// GuardRule matches a runtime permission request. Command rules see each shell command segment of an execute request; others see the raw text.
type GuardRule struct {
	Name    string   `yaml:"name"`
	Kinds   []string `yaml:"kinds"`
	Pattern string   `yaml:"pattern"`
	Command bool     `yaml:"command"`
	// Action is Ask or Deny; empty means Ask.
	Action Action `yaml:"action"`
}

type GuardRequest struct {
	Kind string
	Text string
}

type GuardVerdict struct {
	Action Action
	Rule   string
}

type compiledRule struct {
	GuardRule
	re *regexp.Regexp
}

type Guard struct{ rules []compiledRule }

func NewGuard(rules []GuardRule) (Guard, error) {
	var g Guard
	for _, r := range rules {
		if r.Name == "" {
			return Guard{}, fmt.Errorf("guard rule %q: name is required", r.Pattern)
		}
		if r.Action == "" {
			r.Action = Ask
		}
		if r.Action != Ask && r.Action != Deny {
			return Guard{}, fmt.Errorf("guard rule %s: action %q must be ask or deny", r.Name, r.Action)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return Guard{}, fmt.Errorf("compile guard rule %s: %w", r.Name, err)
		}
		g.rules = append(g.rules, compiledRule{GuardRule: r, re: re})
	}
	return g, nil
}

const gitPrefix = `^git(?:\s+(?:-[Cc]\s+\S+|--(?:git-dir|work-tree|namespace|exec-path|config-env)\s+\S+|--\S+|-[pP]))*\s+`

// StarterRules is the guarded list of CLAUDE.md; matching is on text, so it is a guard rail and not a guarantee.
func StarterRules() []GuardRule {
	exec := []string{KindExecute}
	return []GuardRule{
		{Name: "delete", Kinds: []string{KindDelete}},
		{Name: "delete", Kinds: exec, Command: true, Pattern: `^(?:rm|rmdir|unlink|shred|trash)(?:\s|$)`},
		{Name: "delete", Kinds: exec, Command: true, Pattern: `^find\b.*(?:\s-delete\b|-exec(?:dir)?\s+(?:\S*/)?rm\b)`},
		{Name: "delete", Kinds: exec, Command: true, Pattern: gitPrefix + `clean\b`},
		{Name: "delete", Kinds: exec, Command: true, Pattern: gitPrefix + `reset\b.*--hard\b`},
		{Name: "delete", Kinds: exec, Pattern: `(?i)\bdrop\s+(?:table|database|schema)\b`},
		{Name: "remote", Kinds: exec, Command: true, Pattern: `^(?:ssh|scp|sftp|mosh)(?:\s|$)`},
		{Name: "remote", Kinds: exec, Command: true, Pattern: `^rsync\s.*\S:\S`},
		{Name: "sudo", Kinds: exec, Command: true, Pattern: `^(?:sudo|doas)(?:\s|$)`},
		{Name: "force-push", Kinds: exec, Command: true, Pattern: gitPrefix + `push\b.*(?:\s-f\b|\s--force(?:-with-lease)?\b|\s\+\S)`},
		{Name: "default-branch-push", Kinds: exec, Command: true, Pattern: gitPrefix + `push\b.*\b(?:main|master)\b`},
		{Name: "credentials", Pattern: `(?:^|[\s/'"=:~])\.env(?:$|\.[\w.-]+|[\s'"/;|&)])|(?:^|[\s/'"=:~])\.ssh(?:$|[\s'"/;|&)])|\bid_(?:rsa|dsa|ecdsa|ed25519)\b|\.aws/credentials|\.netrc\b|\.gnupg\b|\.git-credentials`},
	}
}

// Verdict is Deny if any rule that denies matches, else Ask if any matches, else Allow.
func (g Guard) Verdict(req GuardRequest) GuardVerdict {
	out := GuardVerdict{Action: Allow}
	var segs []string
	if req.Kind == KindExecute {
		segs = commandSegments(req.Text, 0)
	}
	for _, r := range g.rules {
		if len(r.Kinds) > 0 && !slices.Contains(r.Kinds, req.Kind) {
			continue
		}
		if !r.matches(req, segs) {
			continue
		}
		if r.Action == Deny {
			return GuardVerdict{Action: Deny, Rule: r.Name}
		}
		if out.Action == Allow {
			out = GuardVerdict{Action: Ask, Rule: r.Name}
		}
	}
	return out
}

func (r compiledRule) matches(req GuardRequest, segs []string) bool {
	if !r.Command {
		return r.re.MatchString(req.Text) || r.re.MatchString(quoteStrip.Replace(req.Text))
	}
	return slices.ContainsFunc(segs, r.re.MatchString)
}

var (
	segmentSplit = regexp.MustCompile("&&|\\|\\||[;|&\\n()`]")
	envAssign    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	shellCFlag   = regexp.MustCompile(`^-[a-z]*c[a-z]*$`)
	quoteStrip   = strings.NewReplacer(`\`, "", `"`, "", `'`, "")
	shells       = []string{"bash", "sh", "zsh", "dash"}
	keywords     = []string{"if", "then", "elif", "else", "while", "until", "do", "!", "{", "coproc"}
	// wrappers run the rest of the line; the value lists the options that consume the next word.
	wrappers = map[string][]string{
		"sudo":    {"-u", "-g", "-h", "-p", "-C", "-T", "-r", "-t", "-U"},
		"doas":    {"-u", "-C"},
		"env":     {"-u", "-C", "-S", "--unset", "--chdir"},
		"command": nil, "nohup": nil, "builtin": nil,
		"time":    {"-f", "-o"},
		"exec":    {"-a"},
		"xargs":   {"-n", "-I", "-P", "-L", "-s", "-d", "-E", "-a"},
		"nice":    {"-n"},
		"timeout": {"-s", "-k"},
	}
)

const maxShellDepth = 3

// commandSegments splits a shell line into commands and adds each with its wrappers peeled, e.g. "sudo rm x" gives "sudo rm x" and "rm x".
func commandSegments(text string, depth int) []string {
	var out []string
	for _, seg := range segmentSplit.Split(text, -1) {
		raw := strings.Fields(seg)
		words := make([]string, len(raw))
		for i, w := range raw {
			words[i] = quoteStrip.Replace(w)
		}
		for len(words) > 0 && envAssign.MatchString(words[0]) {
			raw, words = raw[1:], words[1:]
		}
		for len(words) > 0 {
			words[0] = path.Base(words[0])
			out = append(out, strings.Join(words, " "))
			if depth < maxShellDepth {
				payload := ""
				if slices.Contains(shells, words[0]) {
					if i := slices.IndexFunc(words[1:], shellCFlag.MatchString); i >= 0 && i+2 < len(raw) {
						payload = strings.Join(raw[i+2:], " ")
					}
				} else if words[0] == "eval" {
					payload = strings.Join(raw[1:], " ")
				}
				if payload != "" {
					out = append(out, commandSegments(strings.Trim(payload, `"'`), depth+1)...)
				}
			}
			opts, isWrapper := wrappers[words[0]]
			if !isWrapper && words[0] != "eval" && !slices.Contains(keywords, words[0]) {
				break
			}
			name := words[0]
			raw, words = raw[1:], words[1:]
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || envAssign.MatchString(words[0])) {
				takesValue := slices.Contains(opts, words[0])
				raw, words = raw[1:], words[1:]
				if takesValue && len(words) > 0 {
					raw, words = raw[1:], words[1:]
				}
			}
			if name == "timeout" && len(words) > 0 {
				raw, words = raw[1:], words[1:]
			}
		}
	}
	return out
}
