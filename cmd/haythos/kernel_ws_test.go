package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

// shortHome keeps the socket path under the 104-byte limit of macOS.
func shortHome(t *testing.T) paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "hk")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p, err := homePaths(func(k string) string {
		if k == "HAYTHOS_HOME" {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("paths: %v", err)
	}
	return p
}

func dialKernel(t *testing.T, ctx context.Context, socket, conv string) *ws.Client {
	t.Helper()
	c, err := ws.Dial(ctx, socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Hello(ctx, "tui", conv, "test"); err != nil {
		t.Fatalf("hello: %v", err)
	}
	if err := c.Send(ctx, ws.Subscribe{Conv: conv}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return c
}

func nextOf(t *testing.T, ctx context.Context, c *ws.Client, typ string) ws.Frame {
	t.Helper()
	f, err := c.Next(ctx)
	for err == nil && f.Type == ws.TypeNotice && f.Kind == string(app.NoticeTool) {
		f, err = c.Next(ctx)
	}
	if err != nil {
		t.Fatalf("waiting for %s: %v", typ, err)
	}
	if f.Type != typ {
		t.Fatalf("frame = %+v, want %s", f, typ)
	}
	return f
}

func TestKernelOverWS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := shortHome(t)
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()

	write := task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "note.add", Args: json.RawMessage(`{"id":"n1"}`)}}}
	tools := testfakes.NewTools(task.ToolSpec{Name: "note.add", Write: true, TargetArg: "id", InputSchema: json.RawMessage(`{"type":"object"}`)})
	discord := &testfakes.Notifier{}
	route := &routeNotifier{other: discord}
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(write, task.AssistantTurn{Text: "noted"})},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.add"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: route, Vault: testfakes.Vault{},
		Policy:      policy.Policy{Tiers: map[string]policy.Tier{"note.add": policy.TierInternalWrite}},
		ApprovalTTL: time.Hour,
	})
	srv := ws.New(ws.Config{SocketPath: p.Socket, Version: "v-test", StartedAt: time.Now(), Channels: []string{"discord", "tui"}}, a)
	route.ws = srv
	serveCtx, stopServe := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(serveCtx) }()
	defer func() {
		stopServe()
		if err := <-errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	waitFor(t, func() bool { _, err := os.Stat(p.Socket); return err == nil })

	var out bytes.Buffer
	if err := runStatus(ctx, p, &out); err != nil || !strings.HasPrefix(out.String(), "running pid ") || !strings.Contains(out.String(), "version v-test channels discord,tui") {
		t.Fatalf("status = %q, %v", out.String(), err)
	}

	clientA := dialKernel(t, ctx, p.Socket, "tui:a")
	nextOf(t, ctx, clientA, ws.TypeHistory)
	clientB := dialKernel(t, ctx, p.Socket, "tui:b")
	nextOf(t, ctx, clientB, ws.TypeHistory)

	if err := clientA.Send(ctx, ws.Input{Conv: "tui:a", Text: "add note n1"}); err != nil {
		t.Fatalf("input: %v", err)
	}
	ask := nextOf(t, ctx, clientA, ws.TypeNotice)
	if ask.Kind != string(app.NoticeApproval) || ask.Approval == nil || ask.Approval.Tool != "note.add" {
		t.Fatalf("approval notice = %+v", ask)
	}

	if err := clientB.Send(ctx, ws.Decide{TaskID: ask.TaskID, ApprovalID: ask.Approval.ID, Approve: true}); err != nil {
		t.Fatalf("decide from b: %v", err)
	}
	if f := nextOf(t, ctx, clientB, ws.TypeError); f.Message == "" {
		t.Fatalf("error frame without message")
	}
	if tools.Count("note.add") != 0 {
		t.Fatalf("tool ran after a foreign approval")
	}

	_ = clientA.Close()
	clientA2 := dialKernel(t, ctx, p.Socket, "tui:a")
	h := nextOf(t, ctx, clientA2, ws.TypeHistory)
	if len(h.Pending) != 1 || h.Pending[0].TaskID != ask.TaskID || h.Pending[0].Approval.ID != ask.Approval.ID {
		t.Fatalf("history pending = %+v, want the open approval", h.Pending)
	}
	if err := clientA2.Send(ctx, ws.Decide{TaskID: ask.TaskID, ApprovalID: ask.Approval.ID, Approve: true}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	done := nextOf(t, ctx, clientA2, ws.TypeNotice)
	if done.Kind != string(app.NoticeMessage) || done.Text != "noted" {
		t.Fatalf("final notice = %+v", done)
	}
	if tools.Count("note.add") != 1 {
		t.Fatalf("note.add ran %d times, want 1", tools.Count("note.add"))
	}
	if len(discord.Notices) != 0 {
		t.Fatalf("discord got %d notices from a tui conversation", len(discord.Notices))
	}
}

type fakePlanModel struct{ app.Model }

func (m fakePlanModel) List(context.Context) ([]app.ModelChoice, error) {
	return []app.ModelChoice{
		{Value: "gpt-test", Name: "GPT Test"},
		{Value: "gpt-conflict", Name: "GPT Conflict"},
	}, nil
}
func (m fakePlanModel) WithModel(name string) app.Model {
	return testfakes.NewModel(task.AssistantTurn{Text: "from " + name})
}

func TestConversationModelOverWS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := shortHome(t)
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	route := &routeNotifier{other: &testfakes.Notifier{}}
	deps := app.Deps{
		Models: map[string]app.Model{
			"claude-sub":               testfakes.NewModel(task.AssistantTurn{Text: "from Claude"}),
			"chatgpt-sub":              fakePlanModel{testfakes.NewModel(task.AssistantTurn{Text: "from ChatGPT"})},
			"chatgpt-sub/gpt-conflict": testfakes.NewModel(task.AssistantTurn{Text: "from API key"}),
		},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "claude-sub"},
		Tools:      testfakes.NewTools(),
		Events:     st,
		Ledger:     st,
		Notifier:   route,
		Vault:      testfakes.Vault{},
		Windows:    map[string]int{"chatgpt-sub": 123_456},
	}
	srv := ws.New(ws.Config{SocketPath: p.Socket, DefaultModel: "claude-sub"}, app.New(deps))
	route.ws = srv
	serveCtx, stopServe := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(serveCtx) }()
	defer func() {
		stopServe()
		if err := <-errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	select {
	case <-srv.Listening():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	send := func(c *ws.Client, f any) {
		t.Helper()
		if err := c.Send(ctx, f); err != nil {
			t.Fatal(err)
		}
	}

	first := dialKernel(t, ctx, p.Socket, "tui:first")
	nextOf(t, ctx, first, ws.TypeHistory)
	send(first, ws.SelectModel{})
	if f := nextOf(t, ctx, first, ws.TypeModel); len(f.Models) != 3 || f.Models[1] != "chatgpt-sub/gpt-test" ||
		f.ModelLabels[f.Models[1]] != "GPT Test" {
		t.Fatalf("choices = %+v, want ChatGPT plan model and provider aliases", f)
	}
	send(first, ws.SelectModel{Model: "chatgpt-sub"})
	if f := nextOf(t, ctx, first, ws.TypeModel); f.Model != "chatgpt-sub" {
		t.Fatalf("selection = %+v, want ChatGPT", f)
	}
	send(first, ws.SelectModel{Model: "unknown"})
	nextOf(t, ctx, first, ws.TypeError)
	send(first, ws.SelectModel{Model: "chatgpt-sub/gpt-conflict"})
	nextOf(t, ctx, first, ws.TypeError)
	_ = first.Close()

	convs, err := app.New(deps).Conversations(ctx, ws.Prefix)
	if err != nil || len(convs) != 1 || convs[0].ReplyTo != "tui:first" {
		t.Fatalf("resumable conversations = %+v, %v", convs, err)
	}
	resumed := dialKernel(t, ctx, p.Socket, "tui:first")
	if f := nextOf(t, ctx, resumed, ws.TypeHistory); f.Model != "chatgpt-sub" {
		t.Fatalf("resumed model = %q, want chatgpt-sub", f.Model)
	}
	send(resumed, ws.Input{Conv: "tui:first", Text: "stale", Model: "claude-sub"})
	nextOf(t, ctx, resumed, ws.TypeError)
	send(resumed, ws.Input{Conv: "tui:first", Text: "from an older client"})
	nextOf(t, ctx, resumed, ws.TypeError)
	send(resumed, ws.Input{Conv: "tui:first", Text: "hello", Model: "chatgpt-sub"})
	if f := nextOf(t, ctx, resumed, ws.TypeNotice); f.Text != "from ChatGPT" {
		t.Fatalf("resumed answer = %+v, want ChatGPT response", f)
	}

	second := dialKernel(t, ctx, p.Socket, "tui:second")
	nextOf(t, ctx, second, ws.TypeHistory)
	send(second, ws.Input{Conv: "tui:second", Text: "hello"})
	if f := nextOf(t, ctx, second, ws.TypeNotice); f.Text != "from Claude" {
		t.Fatalf("other conversation answer = %+v, want Claude response", f)
	}
	send(second, ws.Subscribe{Conv: "tui:second", Model: "chatgpt-sub"})
	if f := nextOf(t, ctx, second, ws.TypeHistory); f.Model != "chatgpt-sub" {
		t.Fatalf("explicit model on subscribe = %q, want chatgpt-sub", f.Model)
	}
	send(second, ws.SelectModel{Model: "chatgpt-sub/gpt-test"})
	if f := nextOf(t, ctx, second, ws.TypeModel); f.Model != "chatgpt-sub/gpt-test" {
		t.Fatalf("selection = %+v, want exact plan model", f)
	}
	send(second, ws.Input{Conv: "tui:second", Text: "hello again", Model: "chatgpt-sub/gpt-test"})
	if f := nextOf(t, ctx, second, ws.TypeNotice); f.Text != "from gpt-test" || f.Window != 123_456 {
		t.Fatalf("variant answer = %+v, want specific model and inherited context window", f)
	}
}

func TestDecideRefusesApprovalOfAnotherTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := shortHome(t)
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	writeA := task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "note.add", Args: json.RawMessage(`{"id":"n1"}`)}}}
	writeB := task.AssistantTurn{Calls: []task.ToolCall{{ID: "c2", Name: "note.add", Args: json.RawMessage(`{"id":"n2"}`)}}}
	tools := testfakes.NewTools(task.ToolSpec{Name: "note.add", Write: true, TargetArg: "id", InputSchema: json.RawMessage(`{"type":"object"}`)})
	route := &routeNotifier{other: &testfakes.Notifier{}}
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(writeA, writeB, task.AssistantTurn{Text: "noted"})},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"note.add"}},
		Tools:      tools, Events: st, Ledger: st, Notifier: route, Vault: testfakes.Vault{},
		Policy:      policy.Policy{Tiers: map[string]policy.Tier{"note.add": policy.TierInternalWrite}},
		ApprovalTTL: time.Hour,
	})
	srv := ws.New(ws.Config{SocketPath: p.Socket, Version: "v-test", StartedAt: time.Now()}, a)
	route.ws = srv
	serveCtx, stopServe := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(serveCtx) }()
	defer func() {
		stopServe()
		if err := <-errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	waitFor(t, func() bool { _, err := os.Stat(p.Socket); return err == nil })

	clientA := dialKernel(t, ctx, p.Socket, "tui:a")
	nextOf(t, ctx, clientA, ws.TypeHistory)
	clientB := dialKernel(t, ctx, p.Socket, "tui:b")
	nextOf(t, ctx, clientB, ws.TypeHistory)
	if err := clientA.Send(ctx, ws.Input{Conv: "tui:a", Text: "add note n1"}); err != nil {
		t.Fatalf("input a: %v", err)
	}
	askA := nextOf(t, ctx, clientA, ws.TypeNotice)
	if err := clientB.Send(ctx, ws.Input{Conv: "tui:b", Text: "add note n1"}); err != nil {
		t.Fatalf("input b: %v", err)
	}
	askB := nextOf(t, ctx, clientB, ws.TypeNotice)
	if askA.TaskID == askB.TaskID || askA.Approval.ID == askB.Approval.ID {
		t.Fatalf("setup: both conversations share a task or approval id")
	}

	if err := clientA.Send(ctx, ws.Decide{TaskID: askA.TaskID, ApprovalID: askB.Approval.ID, Approve: true}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if f := nextOf(t, ctx, clientA, ws.TypeError); f.Message == "" {
		t.Fatalf("error frame without message")
	}
	if n := tools.Count("note.add"); n != 0 {
		t.Fatalf("tool ran %d times after an approval id from another task", n)
	}
}

func TestStatusNotRunning(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runStatus(context.Background(), shortHome(t), &out); err != nil {
		t.Fatalf("status: %v", err)
	}
	if out.String() != "not running\n" {
		t.Fatalf("status = %q, want not running", out.String())
	}
}

func TestStopNotRunning(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runStop(context.Background(), shortHome(t), &out); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if out.String() != "not running\n" {
		t.Fatalf("stop = %q, want not running", out.String())
	}
}

type nopHandler struct{}

func (nopHandler) HandleInput(context.Context, app.Input) (string, error)     { return "", nil }
func (nopHandler) HandleApproval(context.Context, string, string, bool) error { return nil }
func (nopHandler) ConversationView(context.Context, string) (app.ConversationView, error) {
	return app.ConversationView{}, nil
}
func (nopHandler) Conversations(context.Context, string) ([]app.ConversationSummary, error) {
	return nil, nil
}
func (nopHandler) TaskOrigin(context.Context, string) (string, error)    { return "", nil }
func (nopHandler) CurrentModel() string                                  { return "" }
func (nopHandler) ChatModels(context.Context) ([]app.ModelChoice, error) { return nil, nil }
func (nopHandler) ConversationModel(context.Context, string) (string, error) {
	return "", nil
}
func (nopHandler) SetConversationModel(context.Context, string, string) error {
	return nil
}

func TestStopIgnoresStalePIDFile(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	if _, err := writePID(p); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	var signalled []int
	var out bytes.Buffer
	err := stopKernel(context.Background(), p, &out, func(pid int, _ syscall.Signal) error {
		signalled = append(signalled, pid)
		return nil
	})
	if err != nil || out.String() != "not running\n" {
		t.Fatalf("stop = %q, %v; want not running", out.String(), err)
	}
	if len(signalled) != 0 {
		t.Fatalf("signalled %v with no kernel on the socket", signalled)
	}
	if _, err := os.Stat(p.PID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale pid file left behind: %v", err)
	}
}

func TestStopSignalsThePIDFromWelcome(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p := shortHome(t)
	srv := ws.New(ws.Config{SocketPath: p.Socket, Version: "v-test", StartedAt: time.Now()}, nopHandler{})
	serveCtx, stopServe := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(serveCtx) }()
	defer func() {
		stopServe()
		if err := <-errc; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	waitFor(t, func() bool { _, err := os.Stat(p.Socket); return err == nil })
	if err := os.MkdirAll(p.Run, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p.PID, []byte("1\n"), 0o600); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	var signalled []int
	var out bytes.Buffer
	err := stopKernel(ctx, p, &out, func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			signalled = append(signalled, pid)
			stopServe()
		}
		return nil
	})
	if err != nil || len(signalled) != 1 || signalled[0] != os.Getpid() {
		t.Fatalf("signalled %v, err %v; want only own pid %d from welcome", signalled, err, os.Getpid())
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition never held")
}
