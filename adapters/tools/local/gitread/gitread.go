// Package gitread gives Alfred read-only git status, diff, log and show inside the chat folder.
package gitread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/readfile"
	"github.com/panca/haythos/core/task"
)

const (
	maxOut     = 16 << 10
	maxScan    = 128 << 10
	defaultLog = 20
	maxLog     = 200
	timeout    = 10 * time.Second
	cutMarker  = "\n[truncated; narrow path or ref]"
)

var (
	// A repo's .git/config is untrusted: nothing it names may run.
	hardened = []string{
		"git", "--no-pager", "--literal-pathspecs",
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null", "-c", "diff.external=", "-c", "core.quotepath=false",
		"--no-optional-locks",
	}
	hardenedEnv = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0"}
	noExec      = []string{"--no-ext-diff", "--no-textconv"}
)

type tool struct{ g readfile.Guard }

func Register(r *local.Registry, cfg readfile.Config) {
	t := tool{g: readfile.NewGuard(cfg)}
	r.Register(spec("git.status", "Read-only git status of the chat folder's repository (branch and changed files).", `{}`), t.status)
	r.Register(spec("git.diff", "Read-only git diff of the working tree, or of staged changes, or against a ref. stat=true lists changed files only.",
		`{"ref":{"type":"string"},"path":{"type":"string","description":"path relative to the chat folder"},"staged":{"type":"boolean"},"stat":{"type":"boolean"}}`), t.diff)
	r.Register(spec("git.log", "Read-only one-line git history, newest first (default 20, at most 200).",
		`{"limit":{"type":"integer"},"path":{"type":"string","description":"path relative to the chat folder"},"ref":{"type":"string"}}`), t.log)
	r.Register(spec("git.show", "Read-only git show of one commit or ref. stat=true lists changed files only.",
		`{"ref":{"type":"string"},"path":{"type":"string","description":"path relative to the chat folder"},"stat":{"type":"boolean"}}`), t.show)
}

func spec(name, desc, props string) task.ToolSpec {
	schema := `{"type":"object","properties":` + props + `}`
	if name == "git.show" {
		schema = `{"type":"object","properties":` + props + `,"required":["ref"]}`
	}
	return task.ToolSpec{Name: name, Description: desc, InputSchema: json.RawMessage(schema)}
}

func refuse(msg string) (task.ToolResult, error) {
	return task.ToolResult{Content: msg, IsError: true}, nil
}

func decode(raw []byte, v any) string {
	if len(raw) == 0 {
		return ""
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return "arguments do not match the tool schema"
	}
	return ""
}

func badRef(ref string) string {
	if strings.HasPrefix(ref, "-") {
		return "ref must not start with '-'"
	}
	if strings.ContainsAny(ref, ":\x00\n") {
		return "ref must not contain ':' or control characters"
	}
	return ""
}

func (t tool) status(ctx context.Context, _ []byte) (task.ToolResult, error) {
	return t.run(ctx, "", true, t.filterStatus, "status", "--porcelain=v1", "-b")
}

func (t tool) diff(ctx context.Context, raw []byte) (task.ToolResult, error) {
	var a struct {
		Ref, Path    string
		Staged, Stat bool
	}
	if why := decode(raw, &a); why != "" {
		return refuse(why)
	}
	a.Ref = strings.TrimSpace(a.Ref)
	if why := badRef(a.Ref); why != "" {
		return refuse(why)
	}
	argv := append([]string{"diff"}, noExec...)
	if a.Staged {
		argv = append(argv, "--cached")
	}
	if a.Stat {
		argv = append(argv, statFlag)
	}
	if a.Ref != "" {
		argv = append(argv, a.Ref)
	}
	return t.run(ctx, a.Path, a.Stat, t.pick(a.Stat), argv...)
}

func (t tool) log(ctx context.Context, raw []byte) (task.ToolResult, error) {
	var a struct {
		Limit     int
		Path, Ref string
	}
	if why := decode(raw, &a); why != "" {
		return refuse(why)
	}
	a.Ref = strings.TrimSpace(a.Ref)
	if why := badRef(a.Ref); why != "" {
		return refuse(why)
	}
	if a.Limit <= 0 {
		a.Limit = defaultLog
	}
	a.Limit = min(a.Limit, maxLog)
	argv := []string{"log", "--oneline", "--decorate", "-n", fmt.Sprint(a.Limit)}
	if a.Ref != "" {
		argv = append(argv, a.Ref)
	}
	return t.run(ctx, a.Path, false, nil, argv...)
}

func (t tool) show(ctx context.Context, raw []byte) (task.ToolResult, error) {
	var a struct {
		Ref, Path string
		Stat      bool
	}
	if why := decode(raw, &a); why != "" {
		return refuse(why)
	}
	a.Ref = strings.TrimSpace(a.Ref)
	if a.Ref == "" {
		return refuse("ref is required")
	}
	if why := badRef(a.Ref); why != "" {
		return refuse(why)
	}
	argv := append([]string{"show"}, noExec...)
	if a.Stat {
		argv = append(argv, statFlag, "--format=%h")
	}
	argv = append(argv, a.Ref)
	return t.run(ctx, a.Path, a.Stat, t.pick(a.Stat), argv...)
}

const statFlag = "--stat=1000,900"

func (t tool) pick(stat bool) func(top, out string) string {
	if stat {
		return t.filterStat
	}
	return t.filterDiff
}

// run executes git with argv (subcommand first), scoped to path inside the chat folder; trusted marks names-only output.
func (t tool) run(ctx context.Context, path string, trusted bool, filter func(top, out string) string, argv ...string) (task.ToolResult, error) {
	if strings.HasPrefix(path, "-") {
		return refuse("path must not start with '-'")
	}
	root, why := t.g.Resolve(ctx, "")
	if why != "" {
		return refuse(why)
	}
	target := root
	if path != "" {
		if target, why = t.g.Resolve(ctx, path); why != "" {
			return refuse(why)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	top, _, err := readfile.Run(ctx, root.Full, git("rev-parse", "--show-toplevel"), hardenedEnv, 4<<10)
	if err != nil {
		return failure(err)
	}
	top = strings.TrimSpace(top)
	out, cut, err := readfile.Run(ctx, root.Full, git(append(argv, "--", target.Rel)...), hardenedEnv, maxScan)
	if err != nil {
		return failure(err)
	}
	if filter != nil {
		out = filter(top, out)
	}
	if len(out) > maxOut {
		out, cut = strings.ToValidUTF8(out[:maxOut], ""), true
	}
	if cut {
		out = strings.ToValidUTF8(out, "") + cutMarker
	}
	return task.ToolResult{Content: task.StripInvisible(out), Untrusted: !trusted}, nil
}

func git(argv ...string) []string { return append(append([]string{}, hardened...), argv...) }

func failure(err error) (task.ToolResult, error) {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return refuse("git is not installed")
	case strings.Contains(err.Error(), "not a git repository"):
		return refuse("the chat folder is not a git repository")
	case errors.Is(err, context.DeadlineExceeded):
		return refuse("git timed out")
	}
	return refuse("git failed: " + err.Error())
}

func (t tool) denied(top string, paths ...string) bool {
	for _, p := range paths {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" && t.g.Denied(filepath.Join(top, p)) {
			return true
		}
	}
	return false
}

func keepLines(out string, drop func(line string) bool) string {
	var kept []string
	for _, l := range strings.Split(out, "\n") {
		if !drop(l) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func (t tool) filterStatus(top, out string) string {
	return keepLines(out, func(l string) bool {
		if strings.HasPrefix(l, "## ") || len(l) < 4 {
			return false
		}
		return t.denied(top, strings.Split(l[3:], " -> ")...)
	})
}

// A stat line is "path | n +-", "old => new" or "dir/{old => new}/file"; every spelling of the path is checked.
func (t tool) filterStat(top, out string) string {
	return keepLines(out, func(l string) bool {
		name, _, ok := strings.Cut(l, " | ")
		if !ok {
			return false
		}
		return t.denied(top, statPaths(name)...)
	})
}

func statPaths(name string) []string {
	open, shut := strings.Index(name, "{"), strings.Index(name, "}")
	if open >= 0 && shut > open {
		if before, after, ok := strings.Cut(name[open+1:shut], " => "); ok {
			return []string{name[:open] + before + name[shut+1:], name[:open] + after + name[shut+1:]}
		}
	}
	return strings.Split(name, " => ")
}

// filterDiff drops each file section whose header names a denied path; a header it cannot parse is dropped too.
func (t tool) filterDiff(top, out string) string {
	var kept []string
	skipping := false
	for _, l := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(l, "diff --git "); ok {
			skipping = t.gitHeaderDenied(top, rest)
		} else if rest, ok := strings.CutPrefix(l, "diff --cc "); ok {
			skipping = t.denied(top, rest)
		} else if rest, ok := strings.CutPrefix(l, "diff --combined "); ok {
			skipping = t.denied(top, rest)
		}
		if !skipping {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

func (t tool) gitHeaderDenied(top, rest string) bool {
	if strings.Contains(rest, `"`) {
		return true
	}
	found := false
	for i := range rest {
		if !strings.HasPrefix(rest[i:], " b/") || !strings.HasPrefix(rest, "a/") {
			continue
		}
		found = true
		if t.denied(top, rest[2:i], rest[i+3:]) {
			return true
		}
	}
	return !found
}
