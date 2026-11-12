package readfile

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Run executes argv without a shell in dir, keeping at most limit bytes of stdout; truncated reports a cut.
func Run(ctx context.Context, dir string, argv, env []string, limit int) (out string, truncated bool, err error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built by callers from fixed flags; no shell
	cmd.Dir = dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)
	cmd.WaitDelay = time.Second
	stdout := &capped{limit: limit}
	cmd.Stdout = stdout
	stderr := &capped{limit: 4 << 10}
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil && stderr.buf.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.buf.Bytes()))
	}
	return stdout.buf.String(), stdout.over, err
}

type capped struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room < len(p) {
		c.over = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}
