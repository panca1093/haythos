package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/panca/haythos/adapters/channel/ws"
)

const sendTimeout = 5 * time.Second

// Conn is the kernel connection the TUI talks through.
type Conn interface {
	Send(ctx context.Context, frame any) error
	Next(ctx context.Context) (ws.Frame, error)
}

type frameMsg ws.Frame

type connErrMsg struct{ err error }

type sendErrMsg struct{ err error }

type entryKind int

const (
	fromUser entryKind = iota
	fromAlfred
	fromKernel
	isError
)

type entry struct {
	kind entryKind
	text string
}

type model struct {
	conn          Conn
	conv, version string
	kernelVersion string
	warning       string
	state         string
	entries       []entry
	pending       []ws.Pending
	input         []rune
	scroll        int
	width, height int
}

var (
	userStyle   = lipgloss.NewStyle().Bold(true)
	alfredStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	dimStyle    = lipgloss.NewStyle().Faint(true)
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	blockStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("3")).Padding(0, 1)
	statusStyle = lipgloss.NewStyle().Reverse(true)
)

// New is the chat screen for one conversation; quitting it never stops the kernel.
func New(c Conn, conv string, version string) tea.Model {
	return &model{conn: c, conv: conv, version: version, state: "idle", width: 80, height: 24}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.handshake(), m.read())
}

func (m *model) handshake() tea.Cmd {
	return func() tea.Msg {
		for _, f := range []any{ws.Hello{Client: "tui", Instance: m.conv, Version: m.version}, ws.Subscribe{Conv: m.conv}} {
			if err := m.send(f); err != nil {
				return sendErrMsg{err}
			}
		}
		return nil
	}
}

func (m *model) read() tea.Cmd {
	return func() tea.Msg {
		f, err := m.conn.Next(context.Background())
		if err != nil {
			return connErrMsg{err}
		}
		return frameMsg(f)
	}
}

func (m *model) send(f any) error {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	return m.conn.Send(ctx, f)
}

func (m *model) sendCmd(f any) tea.Cmd {
	return func() tea.Msg {
		if err := m.send(f); err != nil {
			return sendErrMsg{err}
		}
		return nil
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case frameMsg:
		m.onFrame(ws.Frame(msg))
		return m, m.read()
	case connErrMsg:
		m.add(isError, "disconnected from the kernel: "+msg.err.Error()+" (Ctrl+C to quit)")
	case sendErrMsg:
		m.add(isError, "could not reach the kernel: "+msg.err.Error())
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m *model) onFrame(f ws.Frame) {
	switch f.Type {
	case ws.TypeWelcome:
		m.kernelVersion = f.Version
		if f.Version != m.version {
			m.warning = fmt.Sprintf(`kernel %s differs from this binary %s; run "haythos stop" and start chat again`, f.Version, m.version)
		}
	case ws.TypeHistory:
		m.entries = nil
		for _, t := range f.Turns {
			m.add(fromUser, t.User)
			m.add(fromAlfred, t.Answer)
		}
		m.pending = nil
		for _, p := range f.Pending {
			m.addPending(p)
		}
		if len(m.pending) > 0 {
			m.state = "waiting_approval"
		}
	case ws.TypeNotice:
		m.onNotice(f)
	case ws.TypeError:
		m.add(isError, f.Message)
	}
}

func (m *model) onNotice(f ws.Frame) {
	switch f.Kind {
	case "approval.request":
		if f.Approval != nil {
			m.addPending(ws.Pending{TaskID: f.TaskID, Approval: *f.Approval})
			m.state = "waiting_approval"
		}
	case "message.done":
		m.add(fromAlfred, f.Text)
		m.state = "done"
	case "task.state":
		m.add(fromKernel, f.Text)
		if f.State != "" {
			m.state = f.State
		}
	default:
		m.add(fromKernel, f.Text)
	}
}

func (m *model) addPending(p ws.Pending) {
	for _, q := range m.pending {
		if q.Approval.ID == p.Approval.ID {
			return
		}
	}
	m.pending = append(m.pending, p)
}

func (m *model) add(k entryKind, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	m.entries = append(m.entries, entry{k, text})
	m.scroll = 0
}

func (m *model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyCtrlC:
		return m, tea.Quit
	case tea.KeyPgUp:
		m.scroll += m.height / 2
		return m, nil
	case tea.KeyPgDown:
		m.scroll = max(0, m.scroll-m.height/2)
		return m, nil
	}
	if len(m.pending) > 0 {
		if k.Type == tea.KeyRunes && len(k.Runes) == 1 && (k.Runes[0] == 'y' || k.Runes[0] == 'n') {
			p := m.pending[0]
			m.pending = m.pending[1:]
			approve := k.Runes[0] == 'y'
			verdict := "rejected"
			if approve {
				verdict, m.state = "approved", "running"
			}
			m.add(fromKernel, verdict+" "+p.Approval.Tool+" on "+p.Approval.Target)
			return m, m.sendCmd(ws.Decide{TaskID: p.TaskID, ApprovalID: p.Approval.ID, Approve: approve})
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(string(m.input))
		m.input = nil
		switch text {
		case "":
			return m, nil
		case "/quit":
			return m, tea.Quit
		}
		m.add(fromUser, text)
		m.state = "running"
		return m, m.sendCmd(ws.Input{Conv: m.conv, Text: text})
	case tea.KeyBackspace:
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	case tea.KeySpace:
		m.input = append(m.input, ' ')
	case tea.KeyRunes:
		m.input = append(m.input, k.Runes...)
	}
	return m, nil
}

func (m *model) View() string {
	w := max(m.width, 20)
	var bottom []string
	if len(m.pending) > 0 {
		p := m.pending[0].Approval
		title := fmt.Sprintf("approval needed · tier %d", p.Tier)
		if p.Escalated {
			title += " (escalated: untrusted content in this task)"
		}
		body := strings.Join([]string{
			warnStyle.Render(title),
			"tool: " + p.Tool + "   target: " + p.Target,
			p.Preview,
			userStyle.Render("[y] approve  [n] reject"),
		}, "\n")
		bottom = append(bottom, blockStyle.Width(w-2).Render(body))
	}
	if m.warning != "" {
		bottom = append(bottom, warnStyle.Width(w).Render(m.warning))
	}
	kv := m.kernelVersion
	if kv == "" {
		kv = "?"
	}
	bottom = append(bottom, statusStyle.Width(w).Render(fmt.Sprintf("kernel %s · %s · %s", kv, m.conv, m.state)))
	prompt := "> " + string(m.input)
	if len(m.pending) > 0 {
		prompt = dimStyle.Render("> answer the approval first (y/n), Ctrl+C quits")
	}
	bottom = append(bottom, prompt)
	foot := strings.Join(bottom, "\n")

	room := m.height - lipgloss.Height(foot)
	lines := m.scrollback(w)
	end := max(0, len(lines)-m.scroll)
	begin := max(0, end-room)
	shown := lines[begin:end]
	for len(shown) < room {
		shown = append([]string{""}, shown...)
	}
	return strings.Join(append(shown, foot), "\n")
}

func (m *model) scrollback(w int) []string {
	var out []string
	wrap := lipgloss.NewStyle().Width(w)
	for _, e := range m.entries {
		var s string
		switch e.kind {
		case fromUser:
			s = userStyle.Render("you: ") + e.text
		case fromAlfred:
			s = alfredStyle.Render("Alfred: ") + e.text
		case fromKernel:
			s = dimStyle.Render("· " + e.text)
		case isError:
			s = errStyle.Render("error: " + e.text)
		}
		out = append(out, strings.Split(wrap.Render(s), "\n")...)
	}
	return out
}
