package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/panca/haythos/app"
)

const (
	writeTimeout = 5 * time.Second
	outboundCap  = 64
	readLimit    = 1 << 20
	probeTimeout = time.Second
	// maxSocketPath is the macOS sun_path limit (104 with the NUL); Linux allows 108.
	maxSocketPath = 103
)

// Handler is the slice of the kernel a WS client may reach.
type Handler interface {
	HandleInput(ctx context.Context, in app.Input) (string, error)
	HandleApproval(ctx context.Context, taskID, approvalID string, approve bool) error
	ConversationView(ctx context.Context, replyTo string) (app.ConversationView, error)
	Conversations(ctx context.Context, prefix string) ([]app.ConversationSummary, error)
	TaskOrigin(ctx context.Context, taskID string) (string, error)
}

type Config struct {
	SocketPath string
	Version    string
	StartedAt  time.Time
	Channels   []string
}

// Server is the one WebSocket door for every interactive client, on a unix socket.
type Server struct {
	cfg   Config
	h     Handler
	mu    sync.Mutex
	conns map[*conn]struct{}
	work  sync.WaitGroup
	up    chan struct{}
}

// conn queues frames for its own writer goroutine, so a stalled client can only ever fill its own queue.
type conn struct {
	ws   *websocket.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
	// mu guards conv, loading and held; held collects notices that arrive while history is being loaded.
	mu      sync.Mutex
	conv    string
	loading bool
	held    [][]byte
}

func New(cfg Config, h Handler) *Server {
	return &Server{cfg: cfg, h: h, conns: map[*conn]struct{}{}, up: make(chan struct{})}
}

// Listening is closed once Serve has bound the socket.
func (s *Server) Listening() <-chan struct{} { return s.up }

// Serve listens until ctx is done, then closes every client and removes the socket.
func (s *Server) Serve(ctx context.Context) error {
	ln, unlock, err := s.listen(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	close(s.up)
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.accept(ctx, w, r) }),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	s.closeAll()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	s.work.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("serve %s: %w", s.cfg.SocketPath, err)
	}
	return nil
}

// listen replaces a socket file nobody answers on, and refuses one a live kernel holds.
// The flock, held until unlock, makes probe, remove and bind atomic against another server on the same path.
func (s *Server) listen(ctx context.Context) (net.Listener, func(), error) {
	path := s.cfg.SocketPath
	if len(path) > maxSocketPath {
		return nil, nil, fmt.Errorf("socket path %s is %d bytes, more than the %d a unix socket allows; set HAYTHOS_HOME to a shorter directory", path, len(path), maxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its exec bit; 0700 is owner-only
		return nil, nil, fmt.Errorf("restrict socket dir: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open socket lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil, fmt.Errorf("a kernel is already running on %s", path)
		}
		return nil, nil, fmt.Errorf("take socket lock: %w", err)
	}
	unlock := func() { _ = lock.Close() }
	ln, err := s.bind(ctx, path)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return ln, unlock, nil
}

func (s *Server) bind(ctx context.Context, path string) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; remove it", path)
		}
		d := net.Dialer{Timeout: probeTimeout}
		if c, err := d.DialContext(ctx, "unix", path); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("a kernel is already running on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// The 0700 directory is what protects the window between bind and this chmod.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restrict socket: %w", err)
	}
	return ln, nil
}

// accept serves one client; kernel is the Serve context, which outlives the client.
func (s *Server) accept(kernel context.Context, w http.ResponseWriter, r *http.Request) {
	wsc, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Warn("ws accept failed", "err", err)
		return
	}
	wsc.SetReadLimit(readLimit)
	c := &conn{ws: wsc, out: make(chan []byte, outboundCap), done: make(chan struct{})}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(kernel)
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); s.writer(ctx, c) }()
	defer func() { s.drop(c); cancel(); <-writerDone }()
	for {
		_, b, err := wsc.Read(ctx)
		if err != nil {
			return
		}
		var f clientFrame
		if err := json.Unmarshal(b, &f); err != nil {
			s.reply(c, errorFrame("frame is not a JSON object"))
			continue
		}
		s.dispatch(ctx, kernel, c, f)
	}
}

func (s *Server) dispatch(ctx, kernel context.Context, c *conn, f clientFrame) {
	switch f.Type {
	case TypeHello:
		s.reply(c, Frame{Type: TypeWelcome, Version: s.cfg.Version, PID: os.Getpid(), StartedAt: s.cfg.StartedAt, Channels: s.cfg.Channels})
	case TypeSubscribe:
		s.subscribe(ctx, c, f.Conv)
	case TypeInput:
		conv := s.convOf(c)
		switch {
		case conv == "":
			s.reply(c, errorFrame("subscribe to a conversation first"))
		case f.Conv != conv:
			s.reply(c, errorFrame("input refused: this connection may only write to "+conv))
		case strings.TrimSpace(f.Text) == "":
			s.reply(c, errorFrame("empty input"))
		default:
			s.background(kernel, c, "input", func(ctx context.Context) error {
				_, err := s.h.HandleInput(ctx, app.Input{Text: f.Text, ReplyTo: conv})
				return err
			})
		}
	case TypeDecide:
		conv := s.convOf(c)
		origin, err := s.h.TaskOrigin(ctx, f.TaskID)
		if conv == "" || err != nil || origin != conv {
			s.reply(c, errorFrame("approval refused: it can only be decided from the conversation that asked for it"))
			return
		}
		s.background(kernel, c, "approval", func(ctx context.Context) error {
			return s.h.HandleApproval(ctx, f.TaskID, f.ApprovalID, f.Approve)
		})
	case TypeSessions:
		convs, err := s.h.Conversations(ctx, Prefix)
		if err != nil {
			slog.Warn("ws list sessions failed", "err", err)
			s.reply(c, errorFrame("could not list sessions"))
			return
		}
		out := Frame{Type: TypeSessionsList, Items: []Session{}}
		for _, cv := range convs {
			out.Items = append(out.Items, Session{Conv: cv.ReplyTo, FirstUser: cv.FirstUser, LastAt: cv.LastAt})
		}
		s.reply(c, out)
	default:
		s.reply(c, errorFrame(fmt.Sprintf("unknown frame type %q", f.Type)))
	}
}

// subscribe sets conv first and holds back notices until the history is queued, so none arrives before it.
func (s *Server) subscribe(ctx context.Context, c *conn, conv string) {
	if !strings.HasPrefix(conv, Prefix) || len(conv) == len(Prefix) {
		s.reply(c, errorFrame("subscribe refused: clients may only open "+Prefix+" conversations"))
		return
	}
	c.mu.Lock()
	c.conv, c.loading, c.held = conv, true, nil
	c.mu.Unlock()
	v, err := s.h.ConversationView(ctx, conv)
	first := historyFrame(conv, v)
	if err != nil {
		slog.Warn("ws conversation view failed", "conv", conv, "err", err)
		first = errorFrame("could not load the conversation")
	}
	b, merr := json.Marshal(first)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loading = false
	held := c.held
	c.held = nil
	if merr != nil {
		slog.Warn("ws history encode failed", "conv", conv)
		return
	}
	for _, b := range append([][]byte{b}, held...) {
		s.enqueue(c, b)
	}
}

// background runs kernel work under the kernel context: closing the TUI never cancels a task.
func (s *Server) background(kernel context.Context, c *conn, what string, fn func(context.Context) error) {
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		if err := fn(kernel); err != nil && kernel.Err() == nil {
			slog.Warn("ws "+what+" failed", "err", err)
			s.reply(c, errorFrame(what+" failed"))
		}
	}()
}

// Publish queues a notice for every connection on its conversation and never waits on a client; no subscriber is not an error.
func (s *Server) Publish(_ context.Context, n app.Notice) error {
	s.mu.Lock()
	targets := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		targets = append(targets, c)
	}
	s.mu.Unlock()
	b, err := json.Marshal(noticeFrame(n))
	if err != nil {
		return fmt.Errorf("encode notice frame: %w", err)
	}
	for _, c := range targets {
		c.mu.Lock()
		switch {
		case c.conv == "" || c.conv != n.ReplyTo:
		case c.loading:
			if len(c.held) >= outboundCap {
				s.drop(c)
			} else {
				c.held = append(c.held, b)
			}
		default:
			s.enqueue(c, b)
		}
		c.mu.Unlock()
	}
	return nil
}

func (s *Server) convOf(c *conn) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conv
}

func (s *Server) reply(c *conn, f Frame) {
	b, err := json.Marshal(f)
	if err != nil {
		slog.Warn("ws frame encode failed", "type", f.Type)
		return
	}
	s.enqueue(c, b)
}

// enqueue never blocks; a client that cannot keep up is closed and gets the history again when it reattaches.
func (s *Server) enqueue(c *conn, b []byte) {
	select {
	case c.out <- b:
	default:
		slog.Warn("ws client too slow; closing it")
		s.drop(c)
	}
}

func (s *Server) writer(ctx context.Context, c *conn) {
	for {
		select {
		case <-c.done:
			return
		case b := <-c.out:
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(wctx, websocket.MessageText, b)
			cancel()
			if err != nil {
				s.drop(c)
				return
			}
		}
	}
}

func (s *Server) drop(c *conn) {
	c.once.Do(func() { close(c.done) })
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.ws.CloseNow()
}

func (s *Server) closeAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = map[*conn]struct{}{}
	s.mu.Unlock()
	for c := range conns {
		_ = c.ws.Close(websocket.StatusGoingAway, "kernel stopping")
	}
}

func errorFrame(msg string) Frame { return Frame{Type: TypeError, Message: msg} }
