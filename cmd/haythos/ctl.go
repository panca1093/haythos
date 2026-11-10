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

// runStop signals the pid the kernel itself reports on its socket; the pid file alone is never trusted.
func runStop(ctx context.Context, p paths, out io.Writer) error {
	return stopKernel(ctx, p, out, func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) })
}

func stopKernel(ctx context.Context, p paths, out io.Writer, kill func(pid int, sig syscall.Signal) error) error {
	dialCtx, cancel := context.WithTimeout(ctx, statusWait)
	defer cancel()
	c, err := ws.Dial(dialCtx, p.Socket)
	if err != nil {
		_ = os.Remove(p.PID)
		_, _ = fmt.Fprintln(out, "not running")
		return nil //nolint:nilerr // an unreachable kernel is the "not running" answer, not a failure
	}
	w, err := c.Hello(dialCtx, "stop", "", binaryVersion())
	_ = c.Close()
	if err != nil {
		return fmt.Errorf("ask kernel pid: %w", err)
	}
	pid := w.PID
	if err := kill(pid, syscall.SIGTERM); err != nil {
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
		if errors.Is(statErr, os.ErrNotExist) || errors.Is(kill(pid, 0), syscall.ESRCH) {
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
// displayOf is the version users see: the short commit only, never the dirty flag or the executable hash.
func displayOf(version string) string {
	v, _, _ := strings.Cut(version, "+")
	v, _, _ = strings.Cut(v, "-")
	return v
}

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

type serving struct {
	done chan struct{}
	err  error
	stop func()
}

// serveAfterStart runs start to completion before the socket opens, so no client input races ledger reconciliation.
func serveAfterStart(ctx context.Context, home paths, srv *ws.Server, start func(context.Context) error) (*serving, error) {
	if err := start(ctx); err != nil {
		return nil, fmt.Errorf("start kernel: %w", err)
	}
	srvCtx, stopSrv := context.WithCancel(ctx)
	s := &serving{done: make(chan struct{})}
	go func() { s.err = srv.Serve(srvCtx); close(s.done) }()
	select {
	case <-srv.Listening():
	case <-s.done:
		stopSrv()
		return nil, s.err
	}
	removePID, err := writePID(home)
	if err != nil {
		stopSrv()
		<-s.done
		return nil, err
	}
	s.stop = func() { stopSrv(); <-s.done; removePID() }
	return s, nil
}
