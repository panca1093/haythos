package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/panca/haythos/adapters/channel/ws"
)

type fakeConn struct {
	mu   sync.Mutex
	sent []any
}

func (c *fakeConn) Send(_ context.Context, frame any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, frame)
	return nil
}

func (c *fakeConn) Next(ctx context.Context) (ws.Frame, error) {
	<-ctx.Done()
	return ws.Frame{}, ctx.Err()
}

func (c *fakeConn) frames() []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.sent...)
}

func start(t *testing.T, version string) (tea.Model, *fakeConn) {
	t.Helper()
	c := &fakeConn{}
	m := New(c, "tui:abc", version)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	return m, c
}

func feed(m tea.Model, f ws.Frame) tea.Model {
	m, _ = m.Update(frameMsg(f))
	return m
}

func key(m tea.Model, k tea.KeyMsg) (tea.Model, tea.Cmd) { return m.Update(k) }

func typeText(m tea.Model, s string) tea.Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if b, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range b {
			run(c)
		}
	}
}

func TestInitSubscribes(t *testing.T) {
	t.Parallel()
	c := &fakeConn{}
	m := New(c, "tui:abc", "v1")
	cmd := m.Init()
	done := make(chan struct{})
	go func() { defer close(done); run(cmd) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(c.frames()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := c.frames()
	if len(got) != 2 {
		t.Fatalf("sent %v, want hello then subscribe", got)
	}
	if h, ok := got[0].(ws.Hello); !ok || h.Client != "tui" || h.Version != "v1" {
		t.Fatalf("first frame = %#v, want hello", got[0])
	}
	if s, ok := got[1].(ws.Subscribe); !ok || s.Conv != "tui:abc" {
		t.Fatalf("second frame = %#v, want subscribe tui:abc", got[1])
	}
}

func TestHistoryRenders(t *testing.T) {
	t.Parallel()
	m, _ := start(t, "v1")
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: "v1"})
	m = feed(m, ws.Frame{Type: ws.TypeHistory, Conv: "tui:abc", Turns: []ws.Turn{{User: "hi there", Answer: "hello, sir"}}})
	v := m.View()
	for _, want := range []string{"hi there", "hello, sir", "kernel v1 · tui:abc"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "differs from this binary") {
		t.Fatalf("same version shows a mismatch warning:\n%s", v)
	}
}

func TestEnterSendsInput(t *testing.T) {
	t.Parallel()
	m, c := start(t, "v1")
	m = typeText(m, "check the vm")
	m, cmd := key(m, tea.KeyMsg{Type: tea.KeyEnter})
	run(cmd)
	got := c.frames()
	if len(got) != 1 {
		t.Fatalf("sent %v, want one input", got)
	}
	if in, ok := got[0].(ws.Input); !ok || in.Conv != "tui:abc" || in.Text != "check the vm" {
		t.Fatalf("frame = %#v, want input on tui:abc", got[0])
	}
	if v := m.View(); !strings.Contains(v, "check the vm") || !strings.Contains(v, "running") {
		t.Fatalf("view after send:\n%s", v)
	}
	_, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
	run(cmd)
	if len(c.frames()) != 1 {
		t.Fatalf("empty enter sent a frame")
	}
}

func TestApprovalBlockAndDecide(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		key     string
		approve bool
	}{{"y", true}, {"n", false}} {
		m, c := start(t, "v1")
		m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "approval.request", TaskID: "chat-1", Text: "post to #infra",
			Approval: &ws.Approval{ID: "ap-9", Tool: "discord.post", Target: "#infra", Tier: 2, Preview: "post to #infra: hello"}})
		v := m.View()
		for _, want := range []string{"discord.post", "#infra", "post to #infra: hello", "[y] approve", "[n] reject", "waiting_approval"} {
			if !strings.Contains(v, want) {
				t.Fatalf("approval view lacks %q:\n%s", want, v)
			}
		}
		m, cmd := key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.key)})
		run(cmd)
		got := c.frames()
		want := ws.Decide{TaskID: "chat-1", ApprovalID: "ap-9", Approve: tt.approve}
		if len(got) != 1 || got[0] != want {
			t.Fatalf("%s: sent %#v, want %#v", tt.key, got, want)
		}
		if strings.Contains(m.View(), "[y] approve") {
			t.Fatalf("%s: approval block still shown after deciding", tt.key)
		}
	}
}

func TestPendingFromHistory(t *testing.T) {
	t.Parallel()
	m, c := start(t, "v1")
	m = feed(m, ws.Frame{Type: ws.TypeHistory, Pending: []ws.Pending{{TaskID: "chat-2", Approval: ws.Approval{ID: "ap-1", Tool: "note.add", Target: "n1", Tier: 1, Preview: "add n1"}}}})
	if !strings.Contains(m.View(), "[y] approve") {
		t.Fatalf("pending approval from history not shown:\n%s", m.View())
	}
	_, cmd := key(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	run(cmd)
	if got := c.frames(); len(got) != 1 || got[0] != (ws.Decide{TaskID: "chat-2", ApprovalID: "ap-1", Approve: true}) {
		t.Fatalf("sent %#v", got)
	}
}

func TestVersionMismatchWarning(t *testing.T) {
	t.Parallel()
	m, _ := start(t, "v2")
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: "v1"})
	want := `kernel v1 differs from this binary v2; run "haythos stop" and start chat again`
	if v := m.View(); strings.Count(v, want) != 1 {
		t.Fatalf("view lacks one warning line %q:\n%s", want, v)
	}
}

func TestQuit(t *testing.T) {
	t.Parallel()
	for _, k := range []string{"/quit", "ctrl+c"} {
		m, c := start(t, "v1")
		var cmd tea.Cmd
		if k == "ctrl+c" {
			_, cmd = key(m, tea.KeyMsg{Type: tea.KeyCtrlC})
		} else {
			m = typeText(m, k)
			_, cmd = key(m, tea.KeyMsg{Type: tea.KeyEnter})
		}
		if cmd == nil {
			t.Fatalf("%s: no command, want tea.Quit", k)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("%s: command is not tea.Quit", k)
		}
		if len(c.frames()) != 0 {
			t.Fatalf("%s: sent %v, want nothing", k, c.frames())
		}
	}
}

func TestNoticesAndErrorsRender(t *testing.T) {
	t.Parallel()
	m, _ := start(t, "v1")
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", TaskID: "t", Text: "all green"})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "task.state", State: "failed", Text: "Task failed: budget"})
	m = feed(m, ws.Frame{Type: ws.TypeError, Message: "approval refused"})
	v := m.View()
	for _, want := range []string{"all green", "Task failed: budget", "approval refused", "failed"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%s", want, v)
		}
	}
}
