package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	writeFile(t, dir, "a.txt", "one\n")
	gitIn(t, dir, "add", ".")
	gitIn(t, dir, "commit", "-qm", "first")
	return dir
}

func TestStateFingerprint(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	state := func() string {
		t.Helper()
		s, err := State(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	clean := state()
	if again := state(); again != clean {
		t.Fatalf("state of an untouched repo changed: %q then %q", clean, again)
	}
	steps := []struct {
		name string
		do   func()
	}{
		{"edit a tracked file", func() { writeFile(t, dir, "a.txt", "two\n") }},
		{"edit the same dirty file again", func() { writeFile(t, dir, "a.txt", "three\n") }},
		{"add an untracked file", func() { writeFile(t, dir, "new.txt", "x") }},
		{"commit everything", func() { gitIn(t, dir, "add", "."); gitIn(t, dir, "commit", "-qm", "second") }},
	}
	seen := map[string]string{clean: "clean"}
	for _, st := range steps {
		st.do()
		got := state()
		if prev, dup := seen[got]; dup {
			t.Errorf("after %q the state equals the one after %q", st.name, prev)
		}
		seen[got] = st.name
	}
}

func TestStateErrors(t *testing.T) {
	if _, err := State(context.Background(), t.TempDir()); err == nil {
		t.Error("a folder that is not a git repo was fingerprinted")
	}
}

func TestStateOfEmptyRepo(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	before, err := State(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "a.txt", "x")
	after, err := State(context.Background(), dir)
	if err != nil || after == before {
		t.Errorf("before=%q after=%q err=%v, want a different state once a file exists", before, after, err)
	}
}

func TestRunCheck(t *testing.T) {
	t.Setenv("SECRET_TOKEN", "leak")
	tests := []struct {
		name     string
		argv     []string
		env      []string
		limit    time.Duration
		wantExit int
		wantErr  bool
		wantOut  string
		notOut   string
	}{
		{"pass", []string{"sh", "-c", "echo hi"}, nil, time.Minute, 0, false, "hi", ""},
		{"exit code", []string{"sh", "-c", "echo bad >&2; exit 3"}, nil, time.Minute, 3, false, "bad", ""},
		{"minimal env", []string{"env"}, []string{"HAYTHOS_TASK=t1"}, time.Minute, 0, false, "HAYTHOS_TASK=t1", "SECRET_TOKEN"},
		{"missing binary", []string{"definitely-not-a-binary-xyz"}, nil, time.Minute, -1, true, "", ""},
		{"timeout", []string{"sleep", "30"}, nil, 200 * time.Millisecond, -1, true, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exit, out, err := RunCheck(context.Background(), t.TempDir(), tc.argv, tc.env, tc.limit)
			if exit != tc.wantExit || (err != nil) != tc.wantErr {
				t.Fatalf("exit=%d err=%v, want exit %d err %v", exit, err, tc.wantExit, tc.wantErr)
			}
			if !strings.Contains(out, tc.wantOut) || (tc.notOut != "" && strings.Contains(out, tc.notOut)) {
				t.Errorf("output = %q, want it to contain %q and not %q", out, tc.wantOut, tc.notOut)
			}
		})
	}
}

func TestRunCheckEnvIsPathHomeUser(t *testing.T) {
	_, out, err := RunCheck(context.Background(), t.TempDir(), []string{"env"}, []string{"HAYTHOS_TASK=t1"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		key, _, _ := strings.Cut(line, "=")
		switch key {
		case "PATH", "HOME", "USER", "HAYTHOS_TASK", "PWD", "SHLVL", "_", "OLDPWD":
		default:
			t.Errorf("unexpected variable %q in the check environment", key)
		}
	}
}

func TestRunCheckKeepsTheTail(t *testing.T) {
	_, out, err := RunCheck(context.Background(), t.TempDir(), []string{"sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x; echo TAIL"}, nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 2*tailKeep || !strings.HasSuffix(strings.TrimSpace(out), "TAIL") {
		t.Errorf("len=%d, want a capped output ending in TAIL", len(out))
	}
}

func TestRunCheckTimeoutKillsTheProcessGroup(t *testing.T) {
	start := time.Now()
	_, out, err := RunCheck(context.Background(), t.TempDir(), []string{"sh", "-c", "sleep 60 & echo $!; wait"}, nil, 300*time.Millisecond)
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("err=%v after %s, want a timeout error soon", err, time.Since(start))
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil {
		t.Fatalf("output %q has no child pid", out)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	t.Errorf("child %d survived the timeout", pid)
}
