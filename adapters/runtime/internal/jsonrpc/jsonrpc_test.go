package jsonrpc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/runtime/internal/jsonrpc"
)

type request struct {
	id     json.RawMessage
	method string
	params string
}

type recorder struct {
	mu       sync.Mutex
	notified []string
	requests chan request
}

func newRecorder() *recorder { return &recorder{requests: make(chan request, 8)} }

func (r *recorder) Notification(method string, params json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notified = append(r.notified, method+" "+string(params))
}

func (r *recorder) Request(id json.RawMessage, method string, params json.RawMessage) {
	r.requests <- request{id, method, string(params)}
}

func (r *recorder) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notified...)
}

// peer is the far end of a connection: frames it reads are what the Conn wrote.
type peer struct {
	in  *bufio.Reader
	out io.WriteCloser
}

func newPair(t *testing.T, h jsonrpc.Handler, opts jsonrpc.Options) (*jsonrpc.Conn, *peer) {
	t.Helper()
	cr, pw := io.Pipe()
	pr, cw := io.Pipe()
	c := jsonrpc.NewConn(cr, cw, h, opts)
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })
	return c, &peer{in: bufio.NewReader(pr), out: pw}
}

func (p *peer) read(t *testing.T) map[string]any {
	t.Helper()
	line, err := p.in.ReadBytes('\n')
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal(line, &f); err != nil {
		t.Fatalf("peer decode %q: %v", line, err)
	}
	return f
}

func (p *peer) write(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.out, s+"\n"); err != nil {
		t.Fatalf("peer write: %v", err)
	}
}

func num(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestCallRoutesRepliesByID(t *testing.T) {
	c, p := newPair(t, newRecorder(), jsonrpc.Options{})
	type out struct {
		res string
		err error
	}
	results := make(chan out, 2)
	call := func(method string) {
		var res string
		err := c.Call(context.Background(), method, map[string]any{"k": method}, &res)
		results <- out{res, err}
	}
	go call("a")
	fa := p.read(t)
	go call("b")
	fb := p.read(t)
	if fa["jsonrpc"] != "2.0" || fa["id"] == fb["id"] {
		t.Fatalf("frames = %v, %v: want jsonrpc 2.0 and distinct ids", fa, fb)
	}
	p.write(t, `{"jsonrpc":"2.0","id":`+num(fb["id"])+`,"result":"rb"}`)
	if o := <-results; o.err != nil || o.res != "rb" {
		t.Fatalf("first result = %+v, want rb", o)
	}
	p.write(t, `{"jsonrpc":"2.0","id":`+num(fa["id"])+`,"result":"ra"}`)
	if o := <-results; o.err != nil || o.res != "ra" {
		t.Fatalf("second result = %+v, want ra", o)
	}
}

func TestCallErrorReply(t *testing.T) {
	c, p := newPair(t, newRecorder(), jsonrpc.Options{})
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "m", nil, nil) }()
	f := p.read(t)
	p.write(t, `{"jsonrpc":"2.0","id":`+num(f["id"])+`,"error":{"code":-32601,"message":"Method not found"}}`)
	var re *jsonrpc.Error
	if err := <-errc; !errors.As(err, &re) || re.Code != -32601 || !strings.Contains(re.Error(), "Method not found") {
		t.Fatalf("err = %v, want *Error -32601", err)
	}
}

func TestNotificationsAreDeliveredInOrder(t *testing.T) {
	rec := newRecorder()
	c, p := newPair(t, rec, jsonrpc.Options{})
	p.write(t, `{"jsonrpc":"2.0","method":"n","params":{"i":1}}`)
	p.write(t, `{"jsonrpc":"2.0","method":"n","params":{"i":2}}`)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "ping", nil, nil) }()
	f := p.read(t)
	p.write(t, `{"jsonrpc":"2.0","id":`+num(f["id"])+`,"result":null}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	got := rec.got()
	if len(got) != 2 || got[0] != `n {"i":1}` || got[1] != `n {"i":2}` {
		t.Errorf("notifications = %q, want both in order before the reply returned", got)
	}
}

func TestServerRequestAnsweredLater(t *testing.T) {
	rec := newRecorder()
	c, p := newPair(t, rec, jsonrpc.Options{})
	p.write(t, `{"jsonrpc":"2.0","id":"srv-7","method":"ask","params":{"x":1}}`)
	req := <-rec.requests
	if req.method != "ask" || string(req.id) != `"srv-7"` || req.params != `{"x":1}` {
		t.Fatalf("request = %+v", req)
	}
	p.write(t, `{"jsonrpc":"2.0","id":8,"method":"ask2"}`)
	req2 := <-rec.requests
	go func() { _ = c.Respond(req2.id, map[string]string{"ok": "2"}) }()
	if f := p.read(t); f["id"] != float64(8) || f["jsonrpc"] != "2.0" {
		t.Errorf("frame = %v, want reply to 8", f)
	}
	go func() { _ = c.RespondError(req.id, -32601, "nope") }()
	f := p.read(t)
	e, _ := f["error"].(map[string]any)
	if f["id"] != "srv-7" || e["code"] != float64(-32601) || e["message"] != "nope" {
		t.Errorf("frame = %v, want error reply to srv-7", f)
	}
}

func TestNotify(t *testing.T) {
	c, p := newPair(t, newRecorder(), jsonrpc.Options{})
	go func() { _ = c.Notify("session/cancel", map[string]string{"sessionId": "s"}) }()
	f := p.read(t)
	if _, hasID := f["id"]; hasID || f["method"] != "session/cancel" {
		t.Errorf("frame = %v, want a notification without id", f)
	}
}

func TestFramesWithoutVersionAreTolerated(t *testing.T) {
	rec := newRecorder()
	c, p := newPair(t, rec, jsonrpc.Options{OmitVersion: true})
	errc := make(chan error, 1)
	var res struct{ N int }
	go func() { errc <- c.Call(context.Background(), "initialize", nil, &res) }()
	f := p.read(t)
	if _, has := f["jsonrpc"]; has {
		t.Errorf("frame = %v, want no jsonrpc field with OmitVersion", f)
	}
	p.write(t, `{"method":"thread/started","params":{}}`)
	p.write(t, `{"id":0,"method":"item/requestApproval","params":{}}`)
	p.write(t, `{"id":`+num(f["id"])+`,"result":{"N":3}}`)
	if err := <-errc; err != nil || res.N != 3 {
		t.Fatalf("call = %v (%+v), want N=3", err, res)
	}
	if got := rec.got(); len(got) != 1 || !strings.HasPrefix(got[0], "thread/started") {
		t.Errorf("notifications = %q", got)
	}
	if req := <-rec.requests; req.method != "item/requestApproval" || string(req.id) != "0" {
		t.Errorf("request = %+v", req)
	}
}

func TestGarbageLinesAreSkipped(t *testing.T) {
	c, p := newPair(t, newRecorder(), jsonrpc.Options{})
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "m", nil, nil) }()
	f := p.read(t)
	p.write(t, `starting up...`)
	p.write(t, ``)
	p.write(t, `{"jsonrpc":"2.0","id":`+num(f["id"])+`,"result":1}`)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestCloseFailsPendingCalls(t *testing.T) {
	c, p := newPair(t, newRecorder(), jsonrpc.Options{})
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "m", nil, nil) }()
	p.read(t)
	_ = p.out.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, jsonrpc.ErrClosed) {
			t.Fatalf("err = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not failed on EOF")
	}
	<-c.Done()
	if err := c.Call(context.Background(), "again", nil, nil); !errors.Is(err, jsonrpc.ErrClosed) {
		t.Errorf("call after close = %v, want ErrClosed", err)
	}
}

func TestCallHonoursContext(t *testing.T) {
	c, p := newPair(t, newRecorder(), jsonrpc.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- c.Call(ctx, "m", nil, nil) }()
	p.read(t)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
