package tui

import (
	"context"
	"errors"
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

var opts = Options{Conv: "tui:abc", Version: "6682326+aa", Display: "6682326", Workdir: "/home/me/proj", Home: "/home/me"}

func start(t *testing.T) (tea.Model, *fakeConn) {
	t.Helper()
	c := &fakeConn{}
	m := New(c, opts)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	return m, c
}

func feed(m tea.Model, f ws.Frame) tea.Model {
	m, _ = m.Update(frameMsg(f))
	return m
}

func typeText(m tea.Model, s string) tea.Model {
	for _, r := range s {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

func enter(m tea.Model) (tea.Model, tea.Cmd) { return m.Update(tea.KeyMsg{Type: tea.KeyEnter}) }

func printed(m tea.Model) string { return strings.Join(m.(*model).printed, "\n") }

// drain runs a command tree, ignoring commands that block on the connection.
func drain(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if b, ok := msg.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range b {
				out = append(out, drain(c)...)
			}
			return out
		}
		return []tea.Msg{msg}
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}

func TestInitSendsHelloAndSubscribeWithFolder(t *testing.T) {
	t.Parallel()
	c := &fakeConn{}
	drain(New(c, opts).Init())
	got := c.frames()
	if len(got) < 2 {
		t.Fatalf("frames = %+v", got)
	}
	if h, ok := got[0].(ws.Hello); !ok || h.Version != opts.Version {
		t.Fatalf("first frame = %+v, want hello with the full build id", got[0])
	}
	if s, ok := got[1].(ws.Subscribe); !ok || s.Conv != "tui:abc" || s.Workdir != "/home/me/proj" {
		t.Fatalf("second frame = %+v, want subscribe with the folder", got[1])
	}
}

func TestModelCommandSendsSelectionWithoutCreatingChatMessage(t *testing.T) {
	t.Parallel()
	m, conn := start(t)
	m, cmd := enter(typeText(m, "/model chatgpt-sub"))
	drain(cmd)
	got := conn.frames()
	if len(got) != 1 || got[0] != (ws.SelectModel{Model: "chatgpt-sub"}) {
		t.Fatalf("frames = %+v, want model selection", got)
	}
	if summary, _ := SummaryOf(m); summary.UserMessages != 0 {
		t.Errorf("model command counted as a chat message")
	}
	m = feed(m, ws.Frame{Type: ws.TypeModel, Model: "chatgpt-sub"})
	if !strings.Contains(m.View(), "chatgpt-sub") {
		t.Errorf("selected model missing from status: %s", m.View())
	}
}

func TestModelPickerSelectsByNameAndCanCancel(t *testing.T) {
	t.Parallel()
	m, conn := start(t)
	m, cmd := enter(typeText(m, "/model"))
	drain(cmd)
	choices := ws.Frame{
		Type:   ws.TypeModel,
		Model:  "claude-sub/sonnet",
		Models: []string{"claude-sub/sonnet", "chatgpt-sub/gpt-test"},
		ModelLabels: map[string]string{
			"claude-sub/sonnet":    "Sonnet",
			"chatgpt-sub/gpt-test": "GPT Test",
		},
	}
	m = feed(m, choices)
	for _, want := range []string{"Sonnet", "GPT Test", "↑/↓", "Esc"} {
		if !strings.Contains(m.View(), want) {
			t.Errorf("picker is missing %q", want)
		}
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd = enter(m)
	drain(cmd)
	if got := conn.frames(); len(got) != 2 || got[0] != (ws.SelectModel{}) || got[1] != (ws.SelectModel{Model: "chatgpt-sub/gpt-test"}) {
		t.Fatalf("frames = %+v, want list then chosen model", got)
	}
	m = feed(m, ws.Frame{Type: ws.TypeModel, Model: "chatgpt-sub/gpt-test"})
	m, cmd = enter(typeText(m, "/model"))
	drain(cmd)
	m = feed(m, choices)
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	drain(cmd)
	if strings.Contains(m.View(), "GPT Test") || len(conn.frames()) != 3 {
		t.Fatalf("Esc did not close picker without selecting: %s, %+v", m.View(), conn.frames())
	}
}

func TestInitAppliesModelFlagOnSubscribe(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{}
	options := opts
	options.Model = "chatgpt-sub"
	drain(New(conn, options).Init())
	got := conn.frames()
	if len(got) != 2 || got[1] != (ws.Subscribe{Conv: opts.Conv, Workdir: opts.Workdir, Model: "chatgpt-sub"}) {
		t.Fatalf("frames = %+v, want subscribe with model override", got)
	}
}

func TestModelFlagRequiresKernelConfirmationBeforeSending(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{}
	options := opts
	options.Model = "chatgpt-sub"
	m := feed(New(conn, options), ws.Frame{Type: ws.TypeHistory, Conv: opts.Conv})
	m, cmd := enter(typeText(m, "hello"))
	drain(cmd)
	if len(conn.frames()) != 0 {
		t.Fatal("sent a message after the kernel ignored the requested model")
	}
	if StartupError(m) == nil {
		t.Fatal("ignored model flag did not fail startup")
	}
	m = feed(New(conn, options), ws.Frame{Type: ws.TypeHistory, Conv: opts.Conv, Model: "chatgpt-sub"})
	m = typeText(m, "hello")
	_, cmd = enter(m)
	drain(cmd)
	if got := conn.frames(); len(got) != 1 || got[0] != (ws.Input{Conv: opts.Conv, Text: "hello", Model: "chatgpt-sub"}) {
		t.Fatalf("frames = %+v, want message after model confirmation", got)
	}
}

func TestOldKernelRejectingModelCommandKeepsChatUsable(t *testing.T) {
	t.Parallel()
	m, conn := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: "old", DefaultModel: "claude-sub"})
	m, cmd := enter(typeText(m, "/model chatgpt-sub"))
	drain(cmd)
	m = feed(m, ws.Frame{Type: ws.TypeError, Message: `unknown frame type "model"`})
	_, cmd = enter(typeText(m, "hello"))
	drain(cmd)
	if got := conn.frames(); len(got) != 2 || got[1] != (ws.Input{Conv: opts.Conv, Text: "hello", Model: "claude-sub"}) {
		t.Fatalf("frames = %+v, want chat to remain usable after explicit rejection", got)
	}
}

func TestWelcomePrintsTheCard(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: opts.Version, Model: "opus", DefaultModel: "claude-sub"})
	out := printed(m)
	for _, want := range []string{"◆ Alfred", "v6682326", "opus · claude-sub", "~/proj"} {
		if !strings.Contains(out, want) {
			t.Errorf("card lacks %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"kernel", "dirty", "+aa"} {
		if strings.Contains(out+m.View(), never) {
			t.Errorf("screen shows %q:\n%s\n%s", never, out, m.View())
		}
	}
}

func TestWelcomeWithProviderDefaultModel(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: opts.Version, DefaultModel: "claude-sub"})
	if out := printed(m); !strings.Contains(out, "default · claude-sub") {
		t.Fatalf("card = %s", out)
	}
}

func TestVersionMismatchWarnsOnce(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: "older+bb"})
	if out := printed(m); strings.Count(out, "haythos stop") != 1 {
		t.Fatalf("want one mismatch warning naming haythos stop:\n%s", out)
	}
}

func TestConversationLines(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeHistory, Turns: []ws.Turn{{User: "hi", Answer: "hello"}}})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "tool.started", Text: "fs.read README.md"})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: "README is fine"})
	out := printed(m)
	for _, want := range []string{"› hi", "● hello", "⎿ fs.read README.md", "● README is fine"} {
		if !strings.Contains(out, want) {
			t.Errorf("transcript lacks %q:\n%s", want, out)
		}
	}
}

func TestMessagesAreSeparatedAndWrapped(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})
	m = feed(m, ws.Frame{Type: ws.TypeHistory, Turns: []ws.Turn{{User: "hi", Answer: "a long answer from Alfred that has to wrap across more than one line"}}})
	p := m.(*model).printed
	if len(p) != 2 || !strings.HasPrefix(p[0], "\n") || !strings.HasPrefix(p[1], "\n") {
		t.Fatalf("each message should start after a blank line: %q", p)
	}
	lines := strings.Split(strings.TrimPrefix(p[1], "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("answer did not wrap at width 40: %q", p[1])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "  ") {
			t.Fatalf("wrapped answer lines should hang under the text, got %q", l)
		}
	}
}

func TestEnterSendsInputAndPrintsIt(t *testing.T) {
	t.Parallel()
	m, c := start(t)
	m = typeText(m, "cek README")
	m, cmd := enter(m)
	drain(cmd)
	var sent ws.Input
	for _, f := range c.frames() {
		if in, ok := f.(ws.Input); ok {
			sent = in
		}
	}
	if sent.Conv != "tui:abc" || sent.Text != "cek README" {
		t.Fatalf("input = %+v", sent)
	}
	if !strings.Contains(printed(m), "› cek README") {
		t.Fatalf("transcript = %s", printed(m))
	}
}

func TestExitWordsQuitWithoutSending(t *testing.T) {
	t.Parallel()
	for _, word := range []string{"exit", "/exit", "/quit", " EXIT "} {
		t.Run(word, func(t *testing.T) {
			t.Parallel()
			m, c := start(t)
			m = typeText(m, word)
			_, cmd := enter(m)
			msgs := drain(cmd)
			quit := false
			for _, msg := range msgs {
				_, quit = msg.(tea.QuitMsg)
			}
			if !quit {
				t.Fatalf("%q did not quit: %+v", word, msgs)
			}
			for _, f := range c.frames() {
				if _, ok := f.(ws.Input); ok {
					t.Fatalf("%q was sent to Alfred", word)
				}
			}
		})
	}
}

func TestApprovalBlock(t *testing.T) {
	t.Parallel()
	m, c := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "approval.request", TaskID: "t1", Approval: &ws.Approval{ID: "ap1", Tool: "discord.post", Target: "#infra", Tier: 2, Preview: "deploy done"}})
	view := m.View()
	for _, want := range []string{"Butuh izin", "tier 2", "discord.post → #infra", "deploy done", "[y] izinkan", "[n] tolak"} {
		if !strings.Contains(view, want) {
			t.Errorf("approval view lacks %q:\n%s", want, view)
		}
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	drain(cmd)
	var d ws.Decide
	for _, f := range c.frames() {
		if x, ok := f.(ws.Decide); ok {
			d = x
		}
	}
	if d.TaskID != "t1" || d.ApprovalID != "ap1" || !d.Approve {
		t.Fatalf("decide = %+v", d)
	}
	if strings.Contains(m.View(), "Butuh izin") {
		t.Fatalf("approval still shown after deciding:\n%s", m.View())
	}
}

func TestViewIsInputAndStatusOnly(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	view := m.View()
	for _, want := range []string{"›", "anya Alfred…", "ctx --"} {
		if !strings.Contains(view, want) {
			t.Errorf("view lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "exit") {
		t.Errorf("view still explains how to exit:\n%s", view)
	}
	if lines := strings.Count(view, "\n") + 1; lines > 6 {
		t.Fatalf("idle view is %d lines; the transcript belongs in the scrollback:\n%s", lines, view)
	}
}

func TestStatusShowsModelAndContextUse(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeWelcome, Version: opts.Version, Model: "opus", DefaultModel: "claude-sub"})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: "hi", Context: 38000, Window: 1000000, Cached: 35000})
	view := m.View()
	for _, want := range []string{"opus", "ctx 38k/1M", "▰▱▱▱▱▱▱▱▱▱", "cache 35k"} {
		if !strings.Contains(view, want) {
			t.Errorf("status lacks %q:\n%s", want, view)
		}
	}
}

func TestStatusShowsNoContextBeforeFirstAnswer(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	if view := m.View(); !strings.Contains(view, "ctx --") {
		t.Fatalf("status lacks %q:\n%s", "ctx --", view)
	}
}

func TestKilo(t *testing.T) {
	t.Parallel()
	for n, want := range map[int]string{999: "999", 1000: "1k", 12400: "12.4k", 200000: "200k", 1000000: "1M", 1500000: "1.5M"} {
		if got := kilo(n); got != want {
			t.Errorf("kilo(%d) = %q, want %q", n, got, want)
		}
	}
}

func key(m tea.Model, k tea.KeyMsg) tea.Model {
	m, _ = m.Update(k)
	return m
}

func TestLineEditing(t *testing.T) {
	t.Parallel()
	left := tea.KeyMsg{Type: tea.KeyLeft}
	for _, tc := range []struct {
		name       string
		keys       []tea.KeyMsg
		want       string
		wantCursor int
	}{
		{"typing", nil, "halo dunia", 10},
		{"left then insert", []tea.KeyMsg{left, left, left, left, left, {Type: tea.KeyRunes, Runes: []rune("x")}}, "halo xdunia", 6},
		{"home and end", []tea.KeyMsg{{Type: tea.KeyHome}, {Type: tea.KeyRunes, Runes: []rune(">")}, {Type: tea.KeyEnd}}, ">halo dunia", 11},
		{"ctrl+a ctrl+e", []tea.KeyMsg{{Type: tea.KeyCtrlA}, {Type: tea.KeyDelete}, {Type: tea.KeyCtrlE}}, "alo dunia", 9},
		{"cmd+backspace clears to start", []tea.KeyMsg{left, left, {Type: tea.KeyCtrlU}}, "ia", 0},
		{"ctrl+k clears to end", []tea.KeyMsg{{Type: tea.KeyCtrlA}, {Type: tea.KeyCtrlK}}, "", 0},
		{"option+backspace deletes a word", []tea.KeyMsg{{Type: tea.KeyBackspace, Alt: true}}, "halo ", 5},
		{"ctrl+w deletes a word", []tea.KeyMsg{{Type: tea.KeyCtrlW}}, "halo ", 5},
		{"option+left moves a word", []tea.KeyMsg{{Type: tea.KeyLeft, Alt: true}, {Type: tea.KeyRunes, Runes: []rune("_")}}, "halo _dunia", 6},
		{"backspace at cursor", []tea.KeyMsg{left, {Type: tea.KeyBackspace}}, "halo duna", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := start(t)
			m = typeText(m, "halo dunia")
			for _, k := range tc.keys {
				m = key(m, k)
			}
			mm := m.(*model)
			if string(mm.input) != tc.want || mm.cursor != tc.wantCursor {
				t.Fatalf("input = %q cursor %d, want %q cursor %d", string(mm.input), mm.cursor, tc.want, tc.wantCursor)
			}
		})
	}
}

func TestSummary(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeHistory, Turns: []ws.Turn{{User: "cek README di folder ini apa yang kurang sebelum rilis besok pagi"}}, UserMessages: 2, ToolCalls: 1})
	m = typeText(m, "lagi")
	m, _ = enter(m)
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "tool.started", Text: "fs.list"})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "approval.request", TaskID: "t1", Approval: &ws.Approval{ID: "ap1", Tool: "x", Target: "y"}})
	s, ok := SummaryOf(m)
	if !ok {
		t.Fatal("no summary")
	}
	if s.Conv != "tui:abc" || s.UserMessages != 3 || s.ToolCalls != 2 || s.Pending != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s.Title != "cek README di folder ini apa yang kurang sebelum r…" {
		t.Fatalf("title = %q", s.Title)
	}
	if s.Started.IsZero() {
		t.Fatal("start time not set")
	}
}

func TestControlSequencesAreStrippedFromUntrustedText(t *testing.T) {
	t.Parallel()
	evil := "x\x1b]52;c;ZXZpbA==\x07y\x1b[2Jz\u009b31m\x1b]8;;http://e\x07"
	m, _ := start(t)
	m = feed(m, ws.Frame{Type: ws.TypeHistory, Turns: []ws.Turn{{User: evil, Answer: evil}}})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "tool.started", Text: "fs.read " + evil})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: evil})
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "approval.request", TaskID: "t", Approval: &ws.Approval{ID: "a", Tool: "x", Target: evil, Preview: evil}})
	m = typeText(m, "hi")
	out := printed(m) + m.View()
	for _, bad := range []string{"\x07", "\u009b", "\x1b]"} {
		if strings.Contains(out, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
	if strings.Contains(strings.ReplaceAll(out, "\x1b[", ""), "\x1b") {
		t.Errorf("output contains a raw ESC not from styling")
	}
}

func withClock(m tea.Model, now *time.Time) tea.Model {
	m.(*model).now = func() time.Time { return *now }
	return m
}

func TestWorkingIndicatorWhileWaiting(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	m, _ := start(t)
	m = withClock(m, &now)
	if strings.Contains(m.View(), "…  (") {
		t.Fatalf("indicator shown while idle:\n%s", m.View())
	}
	m, _ = enter(typeText(m, "hi"))
	if !m.(*model).ticking {
		t.Fatal("sending a message must start the indicator ticker")
	}
	first := m.View()
	if !strings.Contains(first, workVerbs[0]+"…") || !strings.Contains(first, "(0s)") {
		t.Fatalf("indicator missing after send:\n%s", first)
	}
	now = now.Add(verbEvery + time.Second)
	m, next := m.Update(tickMsg{})
	if next == nil {
		t.Fatal("ticker must keep running while waiting")
	}
	later := m.View()
	if !strings.Contains(later, workVerbs[1]+"…") || !strings.Contains(later, "(5s)") || later == first {
		t.Fatalf("indicator did not advance:\n%s", later)
	}
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: "hello"})
	if strings.Contains(m.View(), "…  (") {
		t.Fatalf("indicator still shown after the answer:\n%s", m.View())
	}
	if _, cmd := m.Update(tickMsg{}); cmd != nil {
		t.Fatal("ticker must stop once idle")
	}
}

func TestWorkingIndicatorHasABlankLineAbove(t *testing.T) {
	t.Parallel()
	m, _ := start(t)
	m, _ = enter(typeText(m, "hi"))
	lines := strings.Split(m.View(), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "" || !strings.Contains(lines[1], workVerbs[0]+"…") {
		t.Fatalf("want a blank line, then the indicator, at the top of the live area:\n%s", m.View())
	}
}

func TestWorkingIndicatorPausesForApproval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	m, _ := start(t)
	m = withClock(m, &now)
	m, _ = enter(typeText(m, "post it"))
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "approval.request", TaskID: "t1", Approval: &ws.Approval{ID: "a1", Tool: "discord.post", Target: "c", Tier: 2}})
	if strings.Contains(m.View(), "…  (") {
		t.Fatalf("indicator shown while waiting for the owner:\n%s", m.View())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !m.(*model).ticking || !strings.Contains(m.View(), "…  (") {
		t.Fatalf("indicator must resume after approving:\n%s", m.View())
	}
}

func TestElapsed(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{0: "0s", 12 * time.Second: "12s", 65 * time.Second: "1m 05s"} {
		if got := elapsed(d); got != want {
			t.Errorf("elapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestWorkingIndicatorStopsOnEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"answer", frameMsg(ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: "ok"})},
		{"failed task", frameMsg(ws.Frame{Type: ws.TypeNotice, Kind: "task.state", State: "failed", Text: "Task failed: budget"})},
		{"kernel error", frameMsg(ws.Frame{Type: ws.TypeError, Message: "input failed"})},
		{"send error", sendErrMsg{err: errors.New("timeout"), input: true}},
		{"disconnect", connErrMsg{err: errors.New("eof")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := start(t)
			m, _ = enter(typeText(m, "hi"))
			m, _ = m.Update(tc.msg)
			if strings.Contains(m.View(), "…  (") {
				t.Fatalf("indicator still shown after %s:\n%s", tc.name, m.View())
			}
		})
	}
}

func TestWorkingIndicatorWaitsForEveryMessageInFlight(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 20, 0, 0, 0, time.UTC)
	m, _ := start(t)
	m = withClock(m, &now)
	m, _ = enter(typeText(m, "one"))
	now = now.Add(3 * time.Second)
	m, _ = enter(typeText(m, "two"))
	if !strings.Contains(m.View(), "(3s)") {
		t.Fatalf("a second message must not reset the timer:\n%s", m.View())
	}
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: "answer one"})
	if !strings.Contains(m.View(), "…  (") {
		t.Fatalf("indicator gone while the second message is still running:\n%s", m.View())
	}
	m = feed(m, ws.Frame{Type: ws.TypeNotice, Kind: "message.done", Text: "answer two"})
	if strings.Contains(m.View(), "…  (") {
		t.Fatalf("indicator still shown after both answers:\n%s", m.View())
	}
}

func TestErrorsNotAboutAMessageKeepTheIndicator(t *testing.T) {
	t.Parallel()
	for _, msg := range []tea.Msg{
		frameMsg(ws.Frame{Type: ws.TypeError, Message: "approval refused: it can only be decided from the conversation that asked for it"}),
		frameMsg(ws.Frame{Type: ws.TypeError, Message: "decide failed"}),
		sendErrMsg{err: errors.New("timeout")},
	} {
		m, _ := start(t)
		m, _ = enter(typeText(m, "hi"))
		m, _ = m.Update(msg)
		if !strings.Contains(m.View(), "…  (") {
			t.Fatalf("indicator hidden by %+v:\n%s", msg, m.View())
		}
	}
}
