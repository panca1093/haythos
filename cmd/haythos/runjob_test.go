package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/app"
)

type jobRecorder struct {
	nopHandler
	mu   sync.Mutex
	ran  []string
	fail error
}

func (h *jobRecorder) RunJob(_ context.Context, name string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ran = append(h.ran, name)
	return "t1", h.fail
}

func serveJobs(t *testing.T, h ws.Handler) paths {
	t.Helper()
	p := shortHome(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv := ws.New(ws.Config{SocketPath: p.Socket, Version: "v-test", StartedAt: time.Now()}, h)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	waitFor(t, func() bool { _, err := os.Stat(p.Socket); return err == nil })
	return p
}

func TestRunJobRemote(t *testing.T) {
	t.Run("kernel running", func(t *testing.T) {
		h := &jobRecorder{}
		p := serveJobs(t, h)
		var out bytes.Buffer
		handled, err := runJobRemote(context.Background(), p, "fix", &out, 300*time.Millisecond)
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.ran) == 1 && h.ran[0] == "fix" })
		if !strings.Contains(out.String(), "fix") {
			t.Errorf("output = %q, want it to name the job", out.String())
		}
	})
	t.Run("kernel refuses the job", func(t *testing.T) {
		p := serveJobs(t, &jobRecorder{fail: errors.New("unknown job")})
		handled, err := runJobRemote(context.Background(), p, "nope", &bytes.Buffer{}, 2*time.Second)
		if !handled || err == nil {
			t.Fatalf("handled=%v err=%v, want handled with an error", handled, err)
		}
	})
	t.Run("no kernel", func(t *testing.T) {
		handled, err := runJobRemote(context.Background(), shortHome(t), "fix", &bytes.Buffer{}, time.Second)
		if handled || err != nil {
			t.Errorf("handled=%v err=%v, want to fall back to a local run", handled, err)
		}
	})
}

func TestRefuseDelegatedInProcess(t *testing.T) {
	jobs := []app.Job{{Name: "plain"}, {Name: "delegated", Runtime: "oc"}}
	if err := refuseDelegatedInProcess(jobs, "plain"); err != nil {
		t.Errorf("plain job refused: %v", err)
	}
	if err := refuseDelegatedInProcess(jobs, "delegated"); err == nil {
		t.Error("delegated job accepted in the in-process fallback")
	}
}
