//go:build integration

package spike

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type frame = map[string]any

type proc struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin interface{ Write([]byte) (int, error) }
	lines chan string
	mu    sync.Mutex
	log   []string
}

func scratchRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	_ = os.WriteFile(filepath.Join(dir, "README.md"), []byte("# scratch\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "victim.txt"), []byte("delete me\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

func childEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER"), "HAYTHOS_TASK=task-spike", "HAYTHOS_SESSION=sess-spike"}, extra...)
}

func start(t *testing.T, dir string, env []string, name string, args ...string) *proc {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, _ := cmd.StdoutPipe()
	errf, _ := os.Create(filepath.Join(t.TempDir(), "stderr"))
	cmd.Stderr = errf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{t: t, cmd: cmd, stdin: in, lines: make(chan string, 4096)}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
	}()
	t.Cleanup(func() {
		_ = in.(interface{ Close() error }).Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		b, _ := os.ReadFile(errf.Name())
		if len(b) > 0 {
			p.record("stderr", trim(string(b), 600))
		}
		p.flush()
	})
	return p
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...(trimmed)"
	}
	return s
}

func (p *proc) record(dir, s string) {
	p.mu.Lock()
	p.log = append(p.log, dir+" "+s)
	p.mu.Unlock()
}

func (p *proc) flush() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d := os.Getenv("SPIKE_OUT"); d != "" {
		_ = os.WriteFile(filepath.Join(d, strings.ReplaceAll(p.t.Name(), "/", "_")+".txt"), []byte(strings.Join(p.log, "\n")+"\n"), 0o644)
	}
}

func (p *proc) send(f frame) {
	b, _ := json.Marshal(f)
	p.record("->", string(b))
	_, _ = p.stdin.Write(append(b, '\n'))
}

func (p *proc) recv(timeout time.Duration) (frame, bool) {
	select {
	case l, ok := <-p.lines:
		if !ok {
			return nil, false
		}
		p.record("<-", l)
		var f frame
		if err := json.Unmarshal([]byte(l), &f); err != nil {
			return frame{"_raw": l}, true
		}
		return f, true
	case <-time.After(timeout):
		return nil, false
	}
}

func str(f frame, path ...string) string {
	var cur any = f
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	if cur == nil {
		return ""
	}
	if s, ok := cur.(string); ok {
		return s
	}
	return fmt.Sprint(cur)
}

func sub(f frame, k string) frame { m, _ := f[k].(map[string]any); return m }

func readFile(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}
