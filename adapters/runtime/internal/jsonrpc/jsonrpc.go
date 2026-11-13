// Package jsonrpc is a newline-delimited JSON-RPC 2.0 connection over a reader and a writer, as used by ACP and codex app-server.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

var ErrClosed = errors.New("jsonrpc: connection closed")

// Error is the error object of a reply.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Handler receives what the peer sends unprompted. Both methods run on the read loop, one frame at a time and in
// arrival order, so they must not block on the peer: a request is answered later with Conn.Respond.
type Handler interface {
	Notification(method string, params json.RawMessage)
	Request(id json.RawMessage, method string, params json.RawMessage)
}

type Options struct {
	// OmitVersion leaves the "jsonrpc" field out of written frames, as codex app-server does.
	OmitVersion bool
}

type frame struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    error
}

type Conn struct {
	w    io.Writer
	h    Handler
	opts Options

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[string]chan reply
	closed  bool

	done chan struct{}
}

// NewConn starts the read loop. It ends when r returns an error or EOF; Done is closed then.
func NewConn(r io.Reader, w io.Writer, h Handler, opts Options) *Conn {
	c := &Conn{w: w, h: h, opts: opts, pending: map[string]chan reply{}, done: make(chan struct{})}
	go c.readLoop(r)
	return c
}

// Done is closed after the last frame was handed to the handler and the connection can carry no more.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = map[string]chan reply{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- reply{err: ErrClosed}
	}
	close(c.done)
}

func (c *Conn) dispatch(line []byte) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		return
	}
	hasID := len(f.ID) > 0 && string(f.ID) != "null"
	switch {
	case f.Method != "" && hasID:
		c.h.Request(f.ID, f.Method, f.Params)
	case f.Method != "":
		c.h.Notification(f.Method, f.Params)
	case hasID:
		c.mu.Lock()
		ch, ok := c.pending[string(f.ID)]
		delete(c.pending, string(f.ID))
		c.mu.Unlock()
		if !ok {
			return
		}
		if f.Error != nil {
			ch <- reply{err: f.Error}
			return
		}
		ch <- reply{result: f.Result}
	}
}

func (c *Conn) write(f frame) error {
	if !c.opts.OmitVersion {
		f.JSONRPC = "2.0"
	}
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write frame: %w", errors.Join(ErrClosed, err))
	}
	return nil
}

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return b, nil
}

// Call sends a request and waits for its reply, decoding the result into result when it is not nil.
// A reply carrying an error object returns it as *Error.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	p, err := marshalParams(params)
	if err != nil {
		return err
	}
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	key := fmt.Sprint(c.nextID)
	c.nextID++
	c.pending[key] = ch
	c.mu.Unlock()
	forget := func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}
	if err := c.write(frame{ID: json.RawMessage(key), Method: method, Params: p}); err != nil {
		forget()
		return err
	}
	select {
	case rep := <-ch:
		if rep.err != nil {
			return rep.err
		}
		if result == nil || len(rep.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(rep.result, result); err != nil {
			return fmt.Errorf("decode result of %s: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		forget()
		return fmt.Errorf("call %s: %w", method, ctx.Err())
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	p, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(frame{Method: method, Params: p})
}

// Respond answers a request the handler received; id is passed back as received.
func (c *Conn) Respond(id json.RawMessage, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	return c.write(frame{ID: id, Result: b})
}

// RespondError answers a request with an error object.
func (c *Conn) RespondError(id json.RawMessage, code int, message string) error {
	return c.write(frame{ID: id, Error: &Error{Code: code, Message: message}})
}
