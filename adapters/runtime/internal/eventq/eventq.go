// Package eventq is the unbounded in-adapter queue between a runtime's read loop and the consumer of Events.
package eventq

import (
	"context"
	"sync"
	"time"

	"github.com/panca/haythos/app"
)

// flushGrace bounds how long a closed queue waits for a consumer that stopped reading.
const flushGrace = 5 * time.Second

type item struct {
	ev   app.RuntimeEvent
	sent chan struct{}
}

// Queue never blocks its producer: a read loop pushes and moves on, a pump goroutine hands events to the consumer in order.
type Queue struct {
	out   chan app.RuntimeEvent
	wake  chan struct{}
	abort chan struct{}
	done  chan struct{}

	mu     sync.Mutex
	items  []item
	closed bool
	once   sync.Once
}

func New(buf int) *Queue {
	q := &Queue{
		out: make(chan app.RuntimeEvent, buf), wake: make(chan struct{}, 1),
		abort: make(chan struct{}), done: make(chan struct{}),
	}
	go q.pump()
	return q
}

func (q *Queue) Events() <-chan app.RuntimeEvent { return q.out }

// Push queues ev; after Close or Abort it is dropped.
func (q *Queue) Push(ev app.RuntimeEvent) { q.push(item{ev: ev}) }

// PushWait queues ev and returns once it was handed to Events, or with an error when ctx ends or the queue is closed without delivering it.
func (q *Queue) PushWait(ctx context.Context, ev app.RuntimeEvent) error {
	it := item{ev: ev, sent: make(chan struct{})}
	if !q.push(it) {
		return context.Canceled
	}
	select {
	case <-it.sent:
		return nil
	case <-q.done:
		select {
		case <-it.sent:
			return nil
		default:
			return context.Canceled
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) push(it item) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.items = append(q.items, it)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return true
}

// Close stops accepting events; what is queued is still delivered for up to flushGrace, then Events closes.
func (q *Queue) Close() {
	q.mu.Lock()
	first := !q.closed
	q.closed = true
	q.mu.Unlock()
	if first {
		time.AfterFunc(flushGrace, q.Abort)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Abort closes the queue and drops what no consumer took.
func (q *Queue) Abort() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.once.Do(func() { close(q.abort) })
}

func (q *Queue) pump() {
	defer close(q.done)
	defer close(q.out)
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			closed := q.closed
			q.mu.Unlock()
			if closed {
				return
			}
			select {
			case <-q.wake:
			case <-q.abort:
				return
			}
			continue
		}
		it := q.items[0]
		q.items[0] = item{}
		q.items = q.items[1:]
		q.mu.Unlock()
		select {
		case q.out <- it.ev:
			if it.sent != nil {
				close(it.sent)
			}
		case <-q.abort:
			return
		}
	}
}
