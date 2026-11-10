package ws_test

import (
	"context"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/app"
)

func TestClientRoundTrip(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := ws.Dial(ctx, r.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	w, err := c.Hello(ctx, "tui", "i1", "v1")
	if err != nil || w.Version != "v1" || w.PID == 0 {
		t.Fatalf("hello = %+v, %v", w, err)
	}
	if err := c.Send(ctx, ws.Subscribe{Conv: "tui:rt"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if f, err := c.Next(ctx); err != nil || f.Type != ws.TypeHistory || f.Conv != "tui:rt" {
		t.Fatalf("history = %+v, %v", f, err)
	}
	if err := c.Send(ctx, ws.Input{Conv: "tui:rt", Text: "hi"}); err != nil {
		t.Fatalf("input: %v", err)
	}
	for {
		if n, _ := r.h.calls(); n == 1 {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("handler never got the input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.srv.Publish(ctx, app.Notice{Kind: app.NoticeMessage, TaskID: "task-1", Text: "hello", ReplyTo: "tui:rt"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	f, err := c.Next(ctx)
	if err != nil || f.Type != ws.TypeNotice || f.Text != "hello" || f.Kind != string(app.NoticeMessage) {
		t.Fatalf("notice = %+v, %v", f, err)
	}
}

func TestDialNoKernel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ws.Dial(ctx, shortDir(t)+"/haythos.sock"); err == nil {
		t.Fatalf("dial without a kernel: want an error")
	}
}
