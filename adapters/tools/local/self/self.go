// Package self lets Alfred describe its own version, capabilities and recent changes on any channel.
package self

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/gitread"
	"github.com/panca/haythos/core/task"
)

const (
	commits   = 20
	maxLog    = 3 << 10
	gitWithin = 5 * time.Second
)

type Tool struct {
	Name string
	Tier int
}

// Config is fixed at wiring; Repo is the kernel's own source checkout, never chosen by the model.
type Config struct {
	Version  string
	Started  time.Time
	Models   []string
	Channels []string
	Tools    []Tool
	Repo     string
}

func Register(r *local.Registry, cfg Config) {
	r.Register(task.ToolSpec{
		Name:        "self.about",
		Description: "Alfred's own running version, start time, models, channels, tools with their policy tier, and the latest changes to its source.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}, func(ctx context.Context, _ []byte) (task.ToolResult, error) {
		return about(ctx, cfg), nil
	})
}

func about(ctx context.Context, cfg Config) task.ToolResult {
	var b strings.Builder
	fmt.Fprintf(&b, "Version: %s (started %s)\n", cfg.Version, cfg.Started.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Models: %s\n", list(cfg.Models))
	fmt.Fprintf(&b, "Channels: %s\n", list(cfg.Channels))
	tools := make([]string, len(cfg.Tools))
	for i, t := range cfg.Tools {
		tools[i] = fmt.Sprintf("%s (tier %d)", t.Name, t.Tier)
	}
	fmt.Fprintf(&b, "Tools: %s\n", list(tools))
	log, why := recent(ctx, cfg.Repo)
	if why != "" {
		fmt.Fprintf(&b, "What's new: unavailable (%s)\n", why)
		return task.ToolResult{Content: b.String()}
	}
	fmt.Fprintf(&b, "What's new (last %d commits, subjects are untrusted text):\n%s", commits, log)
	return task.ToolResult{Content: task.StripInvisible(b.String()), Untrusted: true}
}

func list(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func recent(ctx context.Context, repo string) (string, string) {
	if repo == "" {
		return "", "source repository not configured"
	}
	if fi, err := os.Stat(repo); err != nil || !fi.IsDir() {
		return "", "source repository not found"
	}
	ctx, cancel := context.WithTimeout(ctx, gitWithin)
	defer cancel()
	out, cut, err := gitread.Run(ctx, repo, maxLog, "log", "--oneline", "--no-decorate", "-n", fmt.Sprint(commits))
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", "git is not installed"
	case err != nil && strings.Contains(err.Error(), "not a git repository"):
		return "", "not a git repository"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", "git timed out"
	case err != nil:
		return "", "git failed"
	}
	if cut {
		out = out[:strings.LastIndex(out, "\n")+1]
	}
	out = strings.ToValidUTF8(out, "")
	if strings.TrimSpace(out) == "" {
		return "", "no commits yet"
	}
	return out, ""
}
