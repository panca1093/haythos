package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestEnsureKernelAlreadyUp(t *testing.T) {
	t.Parallel()
	spawned := false
	err := ensureKernel(context.Background(), shortHome(t),
		func() error { spawned = true; return nil },
		func(context.Context) error { return nil })
	if err != nil || spawned {
		t.Fatalf("err = %v, spawned = %v; want nil and no spawn", err, spawned)
	}
}

func TestEnsureKernelSpawnsOnce(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	var spawns atomic.Int32
	var up atomic.Bool
	spawn := func() error {
		spawns.Add(1)
		go func() { time.Sleep(300 * time.Millisecond); up.Store(true) }()
		return nil
	}
	dial := func(context.Context) error {
		if up.Load() {
			return nil
		}
		return errors.New("connection refused")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ensureKernel(context.Background(), p, spawn, dial)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ensure: %v", err)
		}
	}
	if n := spawns.Load(); n != 1 {
		t.Fatalf("spawned %d kernels, want exactly 1", n)
	}
}

func TestEnsureKernelTimeout(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := ensureKernel(ctx, p, func() error { return nil }, func(context.Context) error { return errors.New("refused") })
	if err == nil || !strings.Contains(err.Error(), filepath.Join(p.Logs, "haythos.log")) {
		t.Fatalf("err = %v, want one naming the log file", err)
	}
}

func TestEnsureKernelLockWaitHonoursContext(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	if err := os.MkdirAll(p.Run, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(p.SpawnLock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	done := make(chan error, 1)
	go func() {
		done <- ensureKernel(ctx, p, func() error { return nil }, func(context.Context) error { return errors.New("refused") })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("ensureKernel still blocked on the lock after the context was cancelled")
	}
}

func TestEnsureKernelSpawnFails(t *testing.T) {
	t.Parallel()
	err := ensureKernel(context.Background(), shortHome(t), func() error { return errors.New("exec failed") }, func(context.Context) error { return errors.New("refused") })
	if err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("err = %v, want the spawn error", err)
	}
}

func TestSpawnKernelDetachesAndLogs(t *testing.T) {
	p := shortHome(t)
	t.Setenv("HAYTHOS_SPAWN_HELPER", "1")
	if err := spawnKernel(p, []string{"-test.run=^TestSpawnHelper$"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	logFile := filepath.Join(p.Logs, "haythos.log")
	waitFor(t, func() bool {
		b, _ := os.ReadFile(logFile)
		return strings.Contains(string(b), "helper: own session, stdin empty")
	})
	if st, err := os.Stat(logFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %v, %v; want 0600", st.Mode(), err)
	}
}

// TestSpawnHelper is the process spawnKernel starts in TestSpawnKernelDetachesAndLogs.
func TestSpawnHelper(t *testing.T) {
	if os.Getenv("HAYTHOS_SPAWN_HELPER") != "1" {
		t.Skip("only runs as the spawned helper")
	}
	if pg := syscall.Getpgrp(); pg != os.Getpid() {
		t.Fatalf("process group = %d, want its own %d (setsid)", pg, os.Getpid())
	}
	if n, _ := os.Stdin.Read(make([]byte, 1)); n != 0 {
		t.Fatalf("stdin is not empty")
	}
	fmt.Println("helper: own session, stdin empty")
}
