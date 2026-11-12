package self_test

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/self"
	"github.com/panca/haythos/core/task"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repoWith(t *testing.T, commits int) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	for i := 1; i <= commits; i++ {
		gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", fmt.Sprintf("change number %d", i))
	}
	return dir
}

func call(t *testing.T, cfg self.Config) task.ToolResult {
	t.Helper()
	r := local.New()
	self.Register(r, cfg)
	if got := r.List([]string{"self.about"}); len(got) != 1 {
		t.Fatalf("self.about not registered: %v", got)
	}
	res, err := r.Call(context.Background(), task.ToolCall{Name: "self.about"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return res
}

func TestAbout(t *testing.T) {
	t.Parallel()
	base := self.Config{
		Version:  "abc1234",
		Started:  time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC),
		Models:   []string{"claude-sub", "gpt"},
		Channels: []string{"discord", "tui"},
		Tools:    []self.Tool{{Name: "file.read", Tier: 0}, {Name: "jobs.define", Tier: 1}},
	}
	cases := []struct {
		name          string
		repo          func(t *testing.T) string
		want, notWant []string
		commits       int
		untrusted     bool
	}{
		{
			name: "shape with a few commits",
			repo: func(t *testing.T) string { return repoWith(t, 3) },
			want: []string{
				"Version: abc1234", "2026-11-12T08:30:00Z",
				"Models: claude-sub, gpt", "Channels: discord, tui",
				"file.read (tier 0)", "jobs.define (tier 1)",
				"change number 3", "change number 1",
			},
			commits:   3,
			untrusted: true,
		},
		{
			name:      "only the last 20 commits",
			repo:      func(t *testing.T) string { return repoWith(t, 25) },
			want:      []string{"change number 25", "change number 6"},
			notWant:   []string{"change number 5\n", "change number 1\n"},
			commits:   20,
			untrusted: true,
		},
		{
			name:    "folder is not a repository",
			repo:    func(t *testing.T) string { return t.TempDir() },
			want:    []string{"Version: abc1234", "unavailable", "not a git repository"},
			notWant: []string{"change number"},
		},
		{
			name:    "folder does not exist",
			repo:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") },
			want:    []string{"Version: abc1234", "unavailable"},
			notWant: []string{"change number"},
		},
		{
			name: "no repository configured",
			repo: func(*testing.T) string { return "" },
			want: []string{"Version: abc1234", "unavailable"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			cfg.Repo = tc.repo(t)
			res := call(t, cfg)
			if res.IsError {
				t.Fatalf("IsError: %s", res.Content)
			}
			for _, w := range tc.want {
				if !strings.Contains(res.Content, w) {
					t.Errorf("output lacks %q:\n%s", w, res.Content)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(res.Content, w) {
					t.Errorf("output has %q:\n%s", w, res.Content)
				}
			}
			if got := strings.Count(res.Content, "change number"); got != tc.commits {
				t.Errorf("commits = %d, want %d", got, tc.commits)
			}
			if res.Untrusted != tc.untrusted {
				t.Errorf("Untrusted = %v, want %v", res.Untrusted, tc.untrusted)
			}
			if len(res.Content) > 4<<10 {
				t.Errorf("output is %d bytes, want at most 4 KiB", len(res.Content))
			}
		})
	}
}

func TestAboutIgnoresRepoConfigPrograms(t *testing.T) {
	t.Parallel()
	dir := repoWith(t, 1)
	marker := filepath.Join(t.TempDir(), "ran")
	gitIn(t, dir, "config", "core.fsmonitor", "touch "+marker)
	gitIn(t, dir, "config", "log.showSignature", "true")
	gitIn(t, dir, "config", "gpg.program", "touch "+marker)
	res := call(t, self.Config{Version: "v", Repo: dir})
	if !strings.Contains(res.Content, "change number 1") {
		t.Fatalf("commit missing:\n%s", res.Content)
	}
	if out, err := exec.CommandContext(t.Context(), "test", "-e", marker).CombinedOutput(); err == nil {
		t.Fatalf("a program from the repo's config ran: %s", out)
	}
}
