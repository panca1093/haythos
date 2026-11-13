//go:build integration

package codexapp_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/runtime/codexapp"
	"github.com/panca/haythos/app"
)

func scratchRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		c := exec.CommandContext(t.Context(), "git", args...)
		c.Dir = dir
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	for name, body := range map[string]string{"README.md": "# scratch\n", "victim.txt": "delete me\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-q", "-m", "init")
	return dir
}

// turn sends prompt and answers every permission request: decline when the text contains "rm ", accept otherwise.
func turn(t *testing.T, sess app.RuntimeSession, prompt string) (stop app.StopReason, asked []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	type result struct {
		stop app.StopReason
		err  error
	}
	done := make(chan result, 1)
	go func() {
		s, err := sess.Send(ctx, prompt)
		done <- result{s, err}
	}()
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				t.Fatal("events closed mid-turn")
			}
			t.Logf("event %s %q tool_kind=%s status=%s", ev.Kind, ev.Text, ev.ToolKind, ev.Status)
			if ev.Kind != app.RuntimePermissionRequest {
				continue
			}
			p := ev.Permission
			asked = append(asked, p.Kind+": "+p.Text)
			option := p.AllowOptionID
			if strings.Contains(p.Text, "rm ") {
				option = p.RejectOptionID
			}
			if err := sess.Approve(ctx, p.RequestID, option); err != nil {
				t.Fatal(err)
			}
		case r := <-done:
			if r.err != nil {
				t.Fatalf("Send: %v", r.err)
			}
			return r.stop, asked
		}
	}
}

func TestCodexAppServer(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not installed")
	}
	dir := scratchRepo(t)
	rt := codexapp.New(codexapp.Config{})
	sess, err := rt.Start(context.Background(), app.RuntimeStart{TaskID: "task-it", SessionID: "sess-it", Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Stop(context.Background()) })
	t.Logf("thread=%s", sess.Ref())

	stop, asked := turn(t, sess, "Use the shell to append the line 'hello from codex' to README.md (for example with printf). Reply 'done' only.")
	t.Logf("turn 1: stop=%s asked=%q", stop, asked)
	if stop != app.StopEndTurn {
		t.Errorf("turn 1 stop = %s, want end_turn", stop)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); !strings.Contains(string(b), "hello from codex") {
		t.Errorf("README.md = %q, want the appended line", b)
	}
	if len(asked) == 0 {
		t.Error("the shell append was not asked about")
	}

	stop, asked = turn(t, sess, "Run `rm victim.txt` with the shell. Reply 'done' only.")
	t.Logf("turn 2: stop=%s asked=%q", stop, asked)
	if len(asked) == 0 {
		t.Error("rm was not asked about")
	}
	if _, err := os.Stat(filepath.Join(dir, "victim.txt")); err != nil {
		t.Errorf("victim.txt was deleted despite the decline: %v", err)
	}
}
