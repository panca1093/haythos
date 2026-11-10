package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
)

func TestServeWaitsForStart(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := shortHome(t)
	srv := ws.New(ws.Config{SocketPath: p.Socket, Version: "v-test", StartedAt: time.Now()}, nopHandler{})
	release := make(chan struct{})
	started := make(chan struct{})
	type result struct {
		s   *serving
		err error
	}
	res := make(chan result, 1)
	go func() {
		s, err := serveAfterStart(ctx, p, srv, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
		res <- result{s, err}
	}()
	<-started
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(p.Socket); err == nil {
		t.Fatalf("socket is listening while Start has not returned")
	}
	if c, err := ws.Dial(ctx, p.Socket); err == nil {
		_ = c.Close()
		t.Fatalf("a client connected before Start returned")
	}
	if _, err := os.Stat(p.PID); err == nil {
		t.Fatalf("pid file written before Start returned")
	}
	close(release)
	r := <-res
	if r.err != nil {
		t.Fatalf("serve: %v", r.err)
	}
	defer r.s.stop()
	if _, err := os.Stat(p.Socket); err != nil {
		t.Fatalf("socket after Start: %v", err)
	}
	if _, err := os.Stat(p.PID); err != nil {
		t.Fatalf("pid file after listen: %v", err)
	}
}

func TestServeAfterStartFailureNeverListens(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	srv := ws.New(ws.Config{SocketPath: p.Socket}, nopHandler{})
	boom := errors.New("boom")
	if _, err := serveAfterStart(context.Background(), p, srv, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the start error", err)
	}
	if _, err := os.Stat(p.Socket); err == nil {
		t.Fatalf("socket created although Start failed")
	}
}
