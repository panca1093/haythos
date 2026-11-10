package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
)

const (
	statusWait = 2 * time.Second
	stopWait   = 10 * time.Second
)

// runStatus asks the kernel itself, so a stale pid file never reads as running.
func runStatus(ctx context.Context, p paths, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, statusWait)
	defer cancel()
	c, err := ws.Dial(ctx, p.Socket)
	if err != nil {
		_, _ = fmt.Fprintln(out, "not running")
		return nil //nolint:nilerr // an unreachable kernel is the "not running" answer, not a failure
	}
	defer func() { _ = c.Close() }()
	w, err := c.Hello(ctx, "status", "", binaryVersion())
	if err != nil {
		return fmt.Errorf("ask kernel status: %w", err)
	}
	_, _ = fmt.Fprintf(out, "running pid %d since %s version %s channels %s\n",
		w.PID, w.StartedAt.Format(time.RFC3339), w.Version, strings.Join(w.Channels, ","))
	return nil
}

// runStop sends SIGTERM to the kernel in the pid file and waits for its socket to go away.
func runStop(ctx context.Context, p paths, out io.Writer) error {
	b, err := os.ReadFile(p.PID)
	if errors.Is(err, os.ErrNotExist) {
		_, _ = fmt.Fprintln(out, "not running")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return fmt.Errorf("pid file %s is corrupt; remove it", p.PID)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			_ = os.Remove(p.PID)
			_, _ = fmt.Fprintln(out, "not running")
			return nil
		}
		return fmt.Errorf("signal kernel pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopWait)
	for time.Now().Before(deadline) {
		_, statErr := os.Stat(p.Socket)
		// A service manager may restart the kernel at once, so a gone pid counts as stopped too.
		if errors.Is(statErr, os.ErrNotExist) || errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			_, _ = fmt.Fprintf(out, "stopped pid %d\n", pid)
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for kernel to stop: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("kernel pid %d did not stop within %s", pid, stopWait)
}

// writePID records the serving kernel for `stop`; the returned func removes it.
func writePID(p paths) (func(), error) {
	if err := os.MkdirAll(p.Run, 0o700); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	if err := os.WriteFile(p.PID, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write pid file: %w", err)
	}
	return func() { _ = os.Remove(p.PID) }, nil
}

// binaryVersion changes with every rebuild, so a client can tell an old kernel is still running.
func binaryVersion() string {
	rev := "dev"
	if bi, ok := debug.ReadBuildInfo(); ok {
		var dirty bool
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value[:min(7, len(s.Value))]
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if dirty {
			rev += "-dirty"
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return rev
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		return rev
	}
	sum := sha256.Sum256(b)
	return rev + "+" + hex.EncodeToString(sum[:4])
}
