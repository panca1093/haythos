//go:build integration

package claudecli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/app"
)

func scratchRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"README.md": "# scratch\n", "victim.txt": "delete me\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"commit", "-q", "-m", "init"}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestRealClaude(t *testing.T) {
	dir := scratchRepo(t)
	sess, err := New(Config{Model: "haiku"}).Start(context.Background(), app.RuntimeStart{TaskID: "task-it", SessionID: "sess-it", Workdir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Stop(context.Background()) }()

	var mu sync.Mutex
	var requests []app.PermissionRequest
	events := 0
	go func() {
		for ev := range sess.Events() {
			mu.Lock()
			events++
			mu.Unlock()
			p := ev.Permission
			if ev.Kind != app.RuntimePermissionRequest || p == nil {
				continue
			}
			mu.Lock()
			requests = append(requests, *p)
			mu.Unlock()
			opt := p.AllowOptionID
			if strings.Contains(p.Text, "rm ") {
				opt = p.RejectOptionID
			}
			if err := sess.Approve(context.Background(), p.RequestID, opt); err != nil {
				t.Errorf("approve: %v", err)
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	stop, err := sess.Send(ctx, "Use the Edit tool to append the line 'hello from haythos' to README.md. Reply 'done' only.")
	if err != nil || stop != app.StopEndTurn {
		t.Fatalf("turn 1 = (%q, %v), want end_turn", stop, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "README.md")); !strings.Contains(string(b), "hello from haythos") {
		t.Errorf("README = %q, want the appended line", b)
	}
	mu.Lock()
	for _, r := range requests {
		if r.Kind == "edit" {
			t.Errorf("the allowed edit asked for permission: %+v", r)
		}
	}
	requests = nil
	mu.Unlock()

	stop, err = sess.Send(ctx, "Run `rm victim.txt` with the Bash tool. Reply 'done' only.")
	if err != nil || stop != app.StopEndTurn {
		t.Fatalf("turn 2 = (%q, %v), want end_turn", stop, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "victim.txt")); err != nil {
		t.Errorf("victim.txt is gone: the denied rm ran (%v)", err)
	}
	mu.Lock()
	defer mu.Unlock()
	asked := false
	for _, r := range requests {
		asked = asked || (r.Kind == "execute" && strings.Contains(r.Text, "rm victim.txt"))
	}
	if !asked {
		t.Errorf("no execute permission request for rm victim.txt; got %+v", requests)
	}
	t.Logf("ref=%s events=%d requests=%+v", sess.Ref(), events, requests)
}
