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
	"time"

	"github.com/coder/websocket"

	"github.com/panca/haythos/app"
)

const (
	writeTimeout = 5 * time.Second
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

type conn struct {
	ws *websocket.Conn
	// wmu serializes writes and guards conv.
	wmu  sync.Mutex
	conv string
}

func New(cfg Config, h Handler) *Server {
	return &Server{cfg: cfg, h: h, conns: map[*conn]struct{}{}, up: make(chan struct{})}
}

// Listening is closed once Serve has bound the socket.
func (s *Server) Listening() <-chan struct{} { return s.up }

// Serve listens until ctx is done, then closes every client and removes the socket.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := s.listen(ctx)
	if err != nil {
		return err
	}
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
func (s *Server) listen(ctx context.Context) (net.Listener, error) {
	path := s.cfg.SocketPath
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("socket path %s is %d bytes, more than the %d a unix socket allows; set HAYTHOS_HOME to a shorter directory", path, len(path), maxSocketPath)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs its exec bit; 0700 is owner-only
		return nil, fmt.Errorf("restrict socket dir: %w", err)
	}
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
	c := &conn{ws: wsc}
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	defer s.drop(c)
	ctx, cancel := context.WithCancel(kernel)
	defer cancel()
	for {
		_, b, err := wsc.Read(ctx)
		if err != nil {
			return
		}
		var f clientFrame
		if err := json.Unmarshal(b, &f); err != nil {
			s.reply(ctx, c, errorFrame("frame is not a JSON object"))
			continue
		}
		s.dispatch(ctx, kernel, c, f)
	}
}

func (s *Server) dispatch(ctx, kernel context.Context, c *conn, f clientFrame) {
	switch f.Type {
	case TypeHello:
		s.reply(ctx, c, Frame{Type: TypeWelcome, Version: s.cfg.Version, PID: os.Getpid(), StartedAt: s.cfg.StartedAt, Channels: s.cfg.Channels})
	case TypeSubscribe:
		s.subscribe(ctx, c, f.Conv)
	case TypeInput:
		conv := s.convOf(c)
		switch {
		case conv == "":
			s.reply(ctx, c, errorFrame("subscribe to a conversation first"))
		case f.Conv != conv:
			s.reply(ctx, c, errorFrame("input refused: this connection may only write to "+conv))
		case strings.TrimSpace(f.Text) == "":
			s.reply(ctx, c, errorFrame("empty input"))
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
			s.reply(ctx, c, errorFrame("approval refused: it can only be decided from the conversation that asked for it"))
			return
		}
		s.background(kernel, c, "approval", func(ctx context.Context) error {
			return s.h.HandleApproval(ctx, f.TaskID, f.ApprovalID, f.Approve)
		})
	case TypeSessions:
		convs, err := s.h.Conversations(ctx, Prefix)
		if err != nil {
			slog.Warn("ws list sessions failed", "err", err)
			s.reply(ctx, c, errorFrame("could not list sessions"))
			return
		}
		out := Frame{Type: TypeSessionsList, Items: []Session{}}
		for _, cv := range convs {
			out.Items = append(out.Items, Session{Conv: cv.ReplyTo, FirstUser: cv.FirstUser, LastAt: cv.LastAt})
		}
		s.reply(ctx, c, out)
	default:
		s.reply(ctx, c, errorFrame(fmt.Sprintf("unknown frame type %q", f.Type)))
	}
}

// subscribe holds the write lock from switching conv to sending history, so no notice can arrive before the history.
func (s *Server) subscribe(ctx context.Context, c *conn, conv string) {
	if !strings.HasPrefix(conv, Prefix) || len(conv) == len(Prefix) {
		s.reply(ctx, c, errorFrame("subscribe refused: clients may only open "+Prefix+" conversations"))
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conv = conv
	v, err := s.h.ConversationView(ctx, conv)
	if err != nil {
		slog.Warn("ws conversation view failed", "conv", conv, "err", err)
		_ = s.write(ctx, c, errorFrame("could not load the conversation"))
		return
	}
	_ = s.write(ctx, c, historyFrame(conv, v))
}

// background runs kernel work under the kernel context: closing the TUI never cancels a task.
func (s *Server) background(kernel context.Context, c *conn, what string, fn func(context.Context) error) {
	s.work.Add(1)
	go func() {
		defer s.work.Done()
		if err := fn(kernel); err != nil && kernel.Err() == nil {
			slog.Warn("ws "+what+" failed", "err", err)
			s.reply(kernel, c, errorFrame(what+" failed: "+err.Error()))
		}
	}()
}

// Publish sends a notice to every connection on its conversation; no subscriber is not an error.
func (s *Server) Publish(ctx context.Context, n app.Notice) error {
	s.mu.Lock()
	var targets []*conn
	for c := range s.conns {
		targets = append(targets, c)
	}
	s.mu.Unlock()
	f := noticeFrame(n)
	for _, c := range targets {
		c.wmu.Lock()
		match := c.conv != "" && c.conv == n.ReplyTo
		var err error
		if match {
			err = s.write(ctx, c, f)
		}
		c.wmu.Unlock()
		if err != nil {
			slog.Warn("ws publish failed; closing client", "err", err)
			s.drop(c)
		}
	}
	return nil
}

func (s *Server) convOf(c *conn) string {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conv
}

func (s *Server) reply(ctx context.Context, c *conn, f Frame) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = s.write(ctx, c, f)
}

// write requires c.wmu.
func (s *Server) write(ctx context.Context, c *conn, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", f.Type, err)
	}
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("write %s frame: %w", f.Type, err)
	}
	return nil
}

func (s *Server) drop(c *conn) {
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
