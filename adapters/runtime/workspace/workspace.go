package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const tailKeep = 16 << 10

func baseEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "USER=" + os.Getenv("USER")}
}

// State fingerprints a git workdir: HEAD plus a hash of the working tree's changes against it.
func State(ctx context.Context, dir string) (string, error) {
	diffs := [][]string{{"diff", "HEAD", "--binary", "--no-ext-diff", "--no-textconv"}}
	head, err := git(ctx, dir, nil, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return "", err
		}
		if _, err := git(ctx, dir, nil, "rev-parse", "--git-dir"); err != nil {
			return "", err
		}
		head = []byte("unborn")
		diffs = [][]string{{"diff", "--cached", "--binary", "--no-ext-diff"}, {"diff", "--binary", "--no-ext-diff"}}
	}
	h := sha256.New()
	for _, args := range append([][]string{{"status", "--porcelain=v1", "-z", "--untracked-files=all"}}, diffs...) {
		if _, err := git(ctx, dir, h, args...); err != nil {
			return "", err
		}
	}
	return strings.TrimSpace(string(head)) + ":" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

func git(ctx context.Context, dir string, sink io.Writer, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed git subcommands from this file
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), "GIT_OPTIONAL_LOCKS=0")
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if sink != nil {
		cmd.Stdout = sink
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errBuf.String()))
	}
	return out.Bytes(), nil
}

// RunCheck runs argv without a shell in dir with PATH, HOME, USER and env only, and returns the tail of stdout and stderr together.
// A non-zero exit is not an error; err is a start failure or a timeout, and the whole process group is killed on timeout.
func RunCheck(ctx context.Context, dir string, argv, env []string, limit time.Duration) (exit int, output string, err error) {
	if len(argv) == 0 {
		return -1, "", errors.New("run check: empty argv")
	}
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is fixed by the job file, never a shell string
	cmd.Dir = dir
	cmd.Env = append(baseEnv(), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	tail := &tailBuffer{keep: tailKeep}
	cmd.Stdout, cmd.Stderr = tail, tail
	runErr := cmd.Run()
	output = tail.String()
	var ee *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return -1, output, fmt.Errorf("timed out after %s", limit)
	case errors.As(runErr, &ee) && ee.ExitCode() >= 0:
		return ee.ExitCode(), output, nil
	case runErr != nil:
		return -1, output, fmt.Errorf("run check %s: %w", argv[0], runErr)
	}
	return 0, output, nil
}

// tailBuffer keeps the last keep bytes written; test failures print last.
type tailBuffer struct {
	keep int
	buf  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - 2*t.keep; over > 0 {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.keep:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	b := t.buf
	if len(b) > t.keep {
		b = b[len(b)-t.keep:]
	}
	return string(b)
}
