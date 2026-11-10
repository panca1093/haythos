package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	lockPoll  = 100 * time.Millisecond
	spawnPoll = 200 * time.Millisecond
	spawnWait = 15 * time.Second
)

// ensureKernel makes sure a kernel answers on the socket; the spawn lock lets only one of many racing clients start it.
func ensureKernel(ctx context.Context, p paths, spawn func() error, dial func(context.Context) error) error {
	if dial(ctx) == nil {
		return nil
	}
	if err := os.MkdirAll(p.Run, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	f, err := os.OpenFile(p.SpawnLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open spawn lock: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := lockContext(ctx, f); err != nil {
		return fmt.Errorf("take spawn lock: %w", err)
	}
	if dial(ctx) == nil {
		return nil
	}
	if err := spawn(); err != nil {
		return fmt.Errorf("start kernel: %w", err)
	}
	logFile := filepath.Join(p.Logs, "haythos.log")
	ctx, cancel := context.WithTimeout(ctx, spawnWait)
	defer cancel()
	t := time.NewTicker(spawnPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("kernel did not answer within %s; see %s", spawnWait, logFile)
		case <-t.C:
			if dial(ctx) == nil {
				return nil
			}
		}
	}
}

// lockContext polls LOCK_NB because a blocking flock cannot be interrupted by ctx.
func lockContext(ctx context.Context, f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// spawnKernel starts `haythos serve` detached from this terminal, logging to <home>/logs/haythos.log.
// The environment is inherited, so HAYTHOS_VAULT_KEY reaches the kernel when it is set.
func spawnKernel(p paths, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find own binary: %w", err)
	}
	if err := os.MkdirAll(p.Logs, 0o700); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logf, err := os.OpenFile(filepath.Join(p.Logs, "haythos.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open kernel log: %w", err)
	}
	defer func() { _ = logf.Close() }()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(exe, args...) //nolint:gosec,noctx // G204: our own binary; the kernel must outlive this client, so no context
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("exec %s serve: %w", exe, err)
	}
	return cmd.Process.Release()
}
