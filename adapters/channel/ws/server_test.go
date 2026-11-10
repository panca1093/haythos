package ws_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/go-cmp/cmp"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
)

type fakeHandler struct {
	mu        sync.Mutex
	inputs    []app.Input
	decisions []string
	views     map[string]app.ConversationView
	convs     []app.ConversationSummary
	origins   map[string]string
	failWith  error
}

func newFakeHandler() *fakeHandler {
	return &fakeHandler{views: map[string]app.ConversationView{}, origins: map[string]string{}}
}

func (h *fakeHandler) HandleInput(_ context.Context, in app.Input) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.inputs = append(h.inputs, in)
	return "task-1", h.failWith
}

func (h *fakeHandler) HandleApproval(_ context.Context, taskID, approvalID string, approve bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.decisions = append(h.decisions, taskID+"/"+approvalID)
	return h.failWith
}

func (h *fakeHandler) ConversationView(_ context.Context, replyTo string) (app.ConversationView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.views[replyTo], nil
}

func (h *fakeHandler) Conversations(_ context.Context, prefix string) ([]app.ConversationSummary, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.convs, nil
}

func (h *fakeHandler) CurrentModel() string { return "opus" }

func (h *fakeHandler) TaskOrigin(_ context.Context, taskID string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	o, ok := h.origins[taskID]
	if !ok {
		return "", errors.New("not found")
	}
	return o, nil
}

func (h *fakeHandler) calls() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.inputs), len(h.decisions)
}

// shortDir keeps socket paths under the 104-byte limit of macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hws")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type rig struct {
	srv    *ws.Server
	h      *fakeHandler
	socket string
	errc   chan error
}

func startServer(t *testing.T, h *fakeHandler) *rig {
	t.Helper()
	socket := filepath.Join(shortDir(t), "run", "haythos.sock")
	srv := ws.New(ws.Config{SocketPath: socket, Version: "v1", DefaultModel: "claude-sub", StartedAt: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), Channels: []string{"discord", "ws"}}, h)
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{srv: srv, h: h, socket: socket, errc: make(chan error, 1)}
	go func() { r.errc <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-r.errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	waitListening(t, socket)
	return r
}

func waitListening(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var d net.Dialer
		if c, err := d.DialContext(context.Background(), "unix", socket); err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server never listened on %s", socket)
}

type testClient struct {
	t *testing.T
	c *websocket.Conn
}

func dialRaw(t *testing.T, socket string) *testClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	c, resp, err := websocket.Dial(ctx, "ws://haythos/", &websocket.DialOptions{HTTPClient: hc})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return &testClient{t: t, c: c}
}

func (c *testClient) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.c.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *testClient) next(wait time.Duration) (ws.Frame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	_, b, err := c.c.Read(ctx)
	if err != nil {
		return ws.Frame{}, err
	}
	var f ws.Frame
	if err := json.Unmarshal(b, &f); err != nil {
		return ws.Frame{}, err
	}
	return f, nil
}

func (c *testClient) expect(typ string) ws.Frame {
	c.t.Helper()
	f, err := c.next(5 * time.Second)
	if err != nil {
		c.t.Fatalf("read %s frame: %v", typ, err)
	}
	if f.Type != typ {
		c.t.Fatalf("frame = %+v, want type %s", f, typ)
	}
	return f
}

func (c *testClient) subscribe(conv string) ws.Frame {
	c.t.Helper()
	c.send(ws.Subscribe{Conv: conv})
	return c.expect(ws.TypeHistory)
}

func TestHelloWelcome(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	c := dialRaw(t, r.socket)
	c.send(ws.Hello{Client: "tui", Instance: "i1", Version: "v2"})
	w := c.expect(ws.TypeWelcome)
	if w.Version != "v1" || !cmp.Equal(w.Channels, []string{"discord", "ws"}) || w.PID != os.Getpid() || w.StartedAt.IsZero() {
		t.Fatalf("welcome = %+v", w)
	}
}

func TestSubscribeSendsHistory(t *testing.T) {
	t.Parallel()
	h := newFakeHandler()
	at := time.Date(2026, 11, 10, 9, 5, 0, 0, time.UTC)
	h.views["tui:a"] = app.ConversationView{
		Turns: []app.TurnView{{User: "hi", Answer: "hello", At: at}},
		Pending: []app.PendingApproval{{TaskID: "t1", Approval: app.ApprovalRequest{
			Approval: policy.Approval{ID: "ap1", Tool: "note.add", Target: "n1", ArgsHash: "secret-hash"},
			Tier:     policy.TierInternalWrite, Preview: "add note n1",
		}}},
	}
	r := startServer(t, h)
	c := dialRaw(t, r.socket)
	got := c.subscribe("tui:a")
	want := ws.Frame{
		Type: ws.TypeHistory, Conv: "tui:a",
		Turns:   []ws.Turn{{User: "hi", Answer: "hello", At: at}},
		Pending: []ws.Pending{{TaskID: "t1", Approval: ws.Approval{ID: "ap1", Tool: "note.add", Target: "n1", Tier: 1, Preview: "add note n1"}}},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("history (-want +got):\n%s", diff)
	}
}

func TestInputRoutesWithOrigin(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	c := dialRaw(t, r.socket)
	c.subscribe("tui:x")
	c.send(ws.Input{Conv: "tui:x", Text: "hello"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.h.mu.Lock()
		got := append([]app.Input(nil), r.h.inputs...)
		r.h.mu.Unlock()
		if len(got) == 1 {
			if want := (app.Input{Text: "hello", ReplyTo: "tui:x"}); got[0] != want {
				t.Fatalf("input = %+v, want %+v", got[0], want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handler never got the input")
}

func TestRejectsForeignOrigin(t *testing.T) {
	t.Parallel()
	h := newFakeHandler()
	h.origins["t-other"] = "tui:other"
	h.origins["t-discord"] = "123456"
	r := startServer(t, h)

	tests := []struct {
		name  string
		conv  string
		frame any
	}{
		{"input for a discord channel", "tui:me", ws.Input{Conv: "123456", Text: "hi"}},
		{"input for another tui conversation", "tui:me", ws.Input{Conv: "tui:other", Text: "hi"}},
		{"input before subscribe", "", ws.Input{Conv: "tui:me", Text: "hi"}},
		{"approval of another tui conversation", "tui:me", ws.Decide{TaskID: "t-other", ApprovalID: "ap", Approve: true}},
		{"approval of a discord task", "tui:me", ws.Decide{TaskID: "t-discord", ApprovalID: "ap", Approve: true}},
		{"approval of an unknown task", "tui:me", ws.Decide{TaskID: "nope", ApprovalID: "ap", Approve: true}},
		{"subscribe to a discord channel", "", ws.Subscribe{Conv: "123456"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := dialRaw(t, r.socket)
			if tt.conv != "" {
				c.subscribe(tt.conv)
			}
			c.send(tt.frame)
			if f := c.expect(ws.TypeError); f.Message == "" {
				t.Fatalf("error frame without a message")
			}
		})
	}
	if in, dec := h.calls(); in != 0 || dec != 0 {
		t.Fatalf("handler called: %d inputs, %d decisions; want none", in, dec)
	}
}

func TestDecideOwnApproval(t *testing.T) {
	t.Parallel()
	h := newFakeHandler()
	h.origins["t-mine"] = "tui:me"
	r := startServer(t, h)
	c := dialRaw(t, r.socket)
	c.subscribe("tui:me")
	c.send(ws.Decide{TaskID: "t-mine", ApprovalID: "ap1", Approve: true})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, dec := h.calls(); dec == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handler never got the decision")
}

func TestPublishOnlyToSubscribers(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	a := dialRaw(t, r.socket)
	a.subscribe("tui:a")
	b := dialRaw(t, r.socket)
	b.subscribe("tui:b")

	n := app.Notice{Kind: app.NoticeMessage, TaskID: "t1", Text: "done", ReplyTo: "tui:a"}
	if err := r.srv.Publish(context.Background(), n); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if f := a.expect(ws.TypeNotice); f.Kind != string(app.NoticeMessage) || f.Text != "done" || f.TaskID != "t1" {
		t.Fatalf("notice = %+v", f)
	}
	if f, err := b.next(200 * time.Millisecond); err == nil {
		t.Fatalf("tui:b got %+v, want nothing", f)
	}
	if err := r.srv.Publish(context.Background(), app.Notice{Kind: app.NoticeMessage, ReplyTo: "tui:nobody"}); err != nil {
		t.Fatalf("publish without subscriber: %v", err)
	}
}

func TestNoticeFrameCarriesContext(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	a := dialRaw(t, r.socket)
	a.subscribe("tui:a")
	n := app.Notice{Kind: app.NoticeMessage, TaskID: "t1", Text: "done", ReplyTo: "tui:a", Context: 38000, Window: 1000000, Cached: 35000}
	if err := r.srv.Publish(context.Background(), n); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if f := a.expect(ws.TypeNotice); f.Context != 38000 || f.Window != 1000000 || f.Cached != 35000 {
		t.Fatalf("notice = %+v, want context 38000 of window 1000000 with 35000 cached", f)
	}
}

func TestStalledClientDoesNotBlockPublish(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	stalled := dialRaw(t, r.socket)
	stalled.subscribe("tui:stalled")
	healthy := dialRaw(t, r.socket)
	healthy.subscribe("tui:healthy")

	big := strings.Repeat("x", 32<<10)
	for i := range 400 {
		start := time.Now()
		if err := r.srv.Publish(context.Background(), app.Notice{Kind: app.NoticeMessage, TaskID: "t", Text: big, ReplyTo: "tui:stalled"}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if d := time.Since(start); d > 100*time.Millisecond {
			t.Fatalf("publish %d took %s with a client that never reads", i, d)
		}
	}
	start := time.Now()
	if err := r.srv.Publish(context.Background(), app.Notice{Kind: app.NoticeMessage, TaskID: "t", Text: "hi", ReplyTo: "tui:healthy"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("publish to the healthy client took %s", d)
	}
	if f := healthy.expect(ws.TypeNotice); f.Text != "hi" {
		t.Fatalf("healthy client got %+v", f)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := stalled.next(time.Until(deadline)); err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("stalled client was never dropped: %v", err)
			}
			return
		}
	}
}

func TestHandlerErrorsAreNotSentToClients(t *testing.T) {
	t.Parallel()
	const secret = "dial tcp 10.0.0.7: token sk-leak refused"
	tests := []struct {
		name  string
		frame any
		want  string
	}{
		{"input", ws.Input{Conv: "tui:me", Text: "hi"}, "input failed"},
		{"approval", ws.Decide{TaskID: "t-mine", ApprovalID: "ap", Approve: true}, "approval failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newFakeHandler()
			h.origins["t-mine"] = "tui:me"
			h.failWith = errors.New(secret)
			r := startServer(t, h)
			c := dialRaw(t, r.socket)
			c.subscribe("tui:me")
			c.send(tt.frame)
			f := c.expect(ws.TypeError)
			if f.Message != tt.want || strings.Contains(f.Message, "sk-leak") {
				t.Fatalf("error message = %q, want %q", f.Message, tt.want)
			}
		})
	}
}

func TestSessionsList(t *testing.T) {
	t.Parallel()
	h := newFakeHandler()
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	h.convs = []app.ConversationSummary{{ReplyTo: "tui:a", FirstUser: "hi", LastAt: at}}
	r := startServer(t, h)
	c := dialRaw(t, r.socket)
	c.send(ws.Sessions{})
	f := c.expect(ws.TypeSessionsList)
	if diff := cmp.Diff([]ws.Session{{Conv: "tui:a", FirstUser: "hi", LastAt: at}}, f.Items); diff != "" {
		t.Fatalf("sessions (-want +got):\n%s", diff)
	}
}

func TestSocketPermissions(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	st, err := os.Stat(r.socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want socket 0600", st.Mode())
	}
	dir, err := os.Stat(filepath.Dir(r.socket))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dir.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dir.Mode().Perm())
	}
}

func TestStaleSocketReplaced(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortDir(t), "run", "haythos.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- ws.New(ws.Config{SocketPath: socket}, newFakeHandler()).Serve(ctx) }()
	waitListening(t, socket)
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("serve over a stale socket: %v", err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket left after serve exits: %v", err)
	}
}

func TestLiveSocketRefused(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := ws.New(ws.Config{SocketPath: r.socket}, newFakeHandler()).Serve(ctx)
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("serve over a live socket = %v, want already running", err)
	}
	waitListening(t, r.socket)
}

func TestSocketLockHeldRefusesSecondServer(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortDir(t), "run", "haythos.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.OpenFile(socket+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock: %v", err)
	}
	err = ws.New(ws.Config{SocketPath: socket}, newFakeHandler()).Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("serve with the lock held = %v, want already running", err)
	}
}

func TestRacingServersOneWins(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortDir(t), "run", "haythos.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errs := make(chan error, 6)
	for range 6 {
		go func() { errs <- ws.New(ws.Config{SocketPath: socket}, newFakeHandler()).Serve(ctx) }()
	}
	for range 5 {
		if err := <-errs; err == nil || !strings.Contains(err.Error(), "already running") {
			t.Fatalf("loser = %v, want already running", err)
		}
	}
	waitListening(t, socket)
	cancel()
	if err := <-errs; err != nil {
		t.Fatalf("winner: %v", err)
	}
}

func TestSocketPathTooLong(t *testing.T) {
	t.Parallel()
	socket := filepath.Join(shortDir(t), strings.Repeat("d", 120), "haythos.sock")
	err := ws.New(ws.Config{SocketPath: socket}, newFakeHandler()).Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HAYTHOS_HOME") {
		t.Fatalf("serve on a %d-byte path = %v, want a hint to shorten HAYTHOS_HOME", len(socket), err)
	}
}

func TestWelcomeNamesTheModel(t *testing.T) {
	t.Parallel()
	r := startServer(t, newFakeHandler())
	c := dialRaw(t, r.socket)
	c.send(ws.Hello{Client: "tui", Instance: "i1", Version: "v1"})
	if w := c.expect(ws.TypeWelcome); w.Model != "opus" || w.DefaultModel != "claude-sub" {
		t.Fatalf("welcome model = %q/%q, want opus/claude-sub", w.Model, w.DefaultModel)
	}
}

func TestInputCarriesTheChatFolder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, sent, want string }{
		{"absolute folder", "/home/me/project", "/home/me/project"},
		{"relative folder is ignored", "project", ""},
		{"no folder", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := startServer(t, newFakeHandler())
			c := dialRaw(t, r.socket)
			c.send(ws.Subscribe{Conv: "tui:x", Workdir: tc.sent})
			c.expect(ws.TypeHistory)
			c.send(ws.Input{Conv: "tui:x", Text: "hello"})
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				r.h.mu.Lock()
				got := append([]app.Input(nil), r.h.inputs...)
				r.h.mu.Unlock()
				if len(got) == 1 {
					if got[0].Workdir != tc.want {
						t.Fatalf("workdir = %q, want %q", got[0].Workdir, tc.want)
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("handler never got the input")
		})
	}
}

func TestHistoryCarriesCounts(t *testing.T) {
	t.Parallel()
	h := newFakeHandler()
	h.views["tui:a"] = app.ConversationView{UserMessages: 3, ToolCalls: 2}
	r := startServer(t, h)
	c := dialRaw(t, r.socket)
	if got := c.subscribe("tui:a"); got.UserMessages != 3 || got.ToolCalls != 2 {
		t.Fatalf("history counts = %d/%d, want 3/2", got.UserMessages, got.ToolCalls)
	}
}
