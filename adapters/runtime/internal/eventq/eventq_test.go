package eventq_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/runtime/internal/eventq"
	"github.com/panca/haythos/app"
)

func ev(i int) app.RuntimeEvent {
	return app.RuntimeEvent{Kind: app.RuntimeMessage, ID: "m", Text: strconv.Itoa(i)}
}

func TestPushNeverBlocksAndKeepsOrderForASlowConsumer(t *testing.T) {
	t.Parallel()
	q := eventq.New(4)
	const n = 5000
	pushed := make(chan struct{})
	go func() {
		for i := range n {
			q.Push(ev(i))
		}
		close(pushed)
	}()
	select {
	case <-pushed:
	case <-time.After(5 * time.Second):
		t.Fatal("Push blocked with no consumer")
	}
	q.Close()
	i := 0
	for e := range q.Events() {
		if e.Text != strconv.Itoa(i) {
			t.Fatalf("event %d is %q", i, e.Text)
		}
		i++
		if i%500 == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if i != n {
		t.Fatalf("got %d events, want %d", i, n)
	}
}

func TestPushWaitReturnsOnlyOnceTheEventWasHandedOver(t *testing.T) {
	t.Parallel()
	q := eventq.New(1)
	for i := range 10 {
		q.Push(ev(i))
	}
	done := make(chan error, 1)
	go func() { done <- q.PushWait(context.Background(), ev(10)) }()
	select {
	case err := <-done:
		t.Fatalf("PushWait returned (%v) while 10 events were still ahead of it and nobody read", err)
	case <-time.After(50 * time.Millisecond):
	}
	for i := range 10 {
		if e := <-q.Events(); e.Text != strconv.Itoa(i) {
			t.Fatalf("event %d is %q", i, e.Text)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PushWait never returned after the consumer caught up")
	}
	if e := <-q.Events(); e.Text != "10" {
		t.Fatalf("last event is %q", e.Text)
	}
}

func TestPushWaitEndsOnContextAndOnClose(t *testing.T) {
	t.Parallel()
	q := eventq.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.PushWait(ctx, ev(0)) }()
	cancel()
	if err := <-done; err == nil {
		t.Error("PushWait ignored its context")
	}

	q2 := eventq.New(0)
	go func() { done <- q2.PushWait(context.Background(), ev(0)) }()
	time.Sleep(20 * time.Millisecond)
	q2.Abort()
	select {
	case err := <-done:
		if err == nil {
			t.Error("PushWait reported delivery after Abort")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PushWait stuck after Abort")
	}
	if _, ok := <-q2.Events(); ok {
		t.Error("Events not closed after Abort")
	}
	q2.Push(ev(1))
}
