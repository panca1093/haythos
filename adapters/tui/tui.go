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

const (
	sendTimeout = 5 * time.Second
	titleRunes  = 50
)

// Conn is the kernel connection the TUI talks through.
type Conn interface {
	Send(ctx context.Context, frame any) error
	Next(ctx context.Context) (ws.Frame, error)
}

// Options: Version is the full build id used to spot a stale kernel; Display is what the user sees.
type Options struct {
	Conv, Version, Display string
	Workdir, Home          string
}

// Summary is what the chat prints after the TUI closes.
type Summary struct {
	Conv, Title             string
	Started                 time.Time
	UserMessages, ToolCalls int
	Pending                 int
}

type frameMsg ws.Frame

type connErrMsg struct{ err error }

type sendErrMsg struct{ err error }

type model struct {
	conn    Conn
	opts    Options
	state   string
	pending []ws.Pending
	input   []rune
	width   int
	printed []string
	carded  bool
	summary Summary
}

var (
	accent    = lipgloss.Color("#e0a458")
	userStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#9ab8e6"))
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#7b828c"))
	nameStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	dotStyle  = lipgloss.NewStyle().Foreground(accent)
	errStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#e06c75"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6c07b"))
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#7fbf7f"))
	ruleStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#4a525c"))
	cardStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#4a525c")).Padding(0, 1)
	askStyle  = lipgloss.NewStyle().Border(lipgloss.ThickBorder(), false, false, false, true).BorderForeground(lipgloss.Color("#e6c07b")).PaddingLeft(1)
)

// New is the chat screen for one conversation; quitting it never stops the kernel.
func New(c Conn, opts Options) tea.Model {
	return &model{conn: c, opts: opts, state: "idle", width: 80, summary: Summary{Conv: opts.Conv, Started: time.Now()}}
}

// SummaryOf returns the exit summary of a model New returned.
func SummaryOf(m tea.Model) (Summary, bool) {
	mm, ok := m.(*model)
	if !ok {
		return Summary{}, false
	}
	s := mm.summary
	s.Pending = len(mm.pending)
	return s, true
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.handshake(), m.read())
}

func (m *model) handshake() tea.Cmd {
	return func() tea.Msg {
		frames := []any{
			ws.Hello{Client: "tui", Instance: m.opts.Conv, Version: m.opts.Version},
			ws.Subscribe{Conv: m.opts.Conv, Workdir: m.opts.Workdir},
		}
		for _, f := range frames {
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

// print adds finished lines to the terminal scrollback above the live input area.
func (m *model) print(s string) tea.Cmd {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	m.printed = append(m.printed, s)
	return tea.Println(s)
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case frameMsg:
		return m, tea.Batch(m.onFrame(ws.Frame(msg)), m.read())
	case connErrMsg:
		return m, m.print(errStyle.Render("Terputus dari Alfred: " + msg.err.Error() + " (Ctrl+C untuk keluar)"))
	case sendErrMsg:
		return m, m.print(errStyle.Render("Tidak bisa menghubungi Alfred: " + msg.err.Error()))
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	return m, nil
}

func (m *model) onFrame(f ws.Frame) tea.Cmd {
	switch f.Type {
	case ws.TypeWelcome:
		var cmds []tea.Cmd
		if !m.carded {
			m.carded = true
			cmds = append(cmds, m.print(m.card(f)))
		}
		if f.Version != m.opts.Version {
			cmds = append(cmds, m.print(warnStyle.Render(`Alfred yang berjalan lebih lama dari aplikasi ini; jalankan "haythos stop" lalu buka chat lagi.`)))
		}
		return tea.Batch(cmds...)
	case ws.TypeHistory:
		m.summary.UserMessages, m.summary.ToolCalls = f.UserMessages, f.ToolCalls
		var cmds []tea.Cmd
		for _, t := range f.Turns {
			m.title(t.User)
			cmds = append(cmds, m.print(userLine(t.User)), m.print(alfredLine(t.Answer)))
		}
		m.pending = nil
		for _, p := range f.Pending {
			m.addPending(p)
		}
		return tea.Batch(cmds...)
	case ws.TypeNotice:
		return m.onNotice(f)
	case ws.TypeError:
		return m.print(errStyle.Render(f.Message))
	}
	return nil
}

func (m *model) onNotice(f ws.Frame) tea.Cmd {
	switch f.Kind {
	case "approval.request":
		if f.Approval != nil {
			m.addPending(ws.Pending{TaskID: f.TaskID, Approval: *f.Approval})
		}
		return nil
	case "tool.started":
		m.summary.ToolCalls++
		return m.print(dimStyle.Render("  ⎿ " + f.Text))
	case "message.done":
		m.state = "done"
		return m.print(alfredLine(f.Text))
	case "task.state":
		if f.State != "" {
			m.state = f.State
		}
		return m.print(dimStyle.Render("  " + f.Text))
	default:
		return m.print(warnStyle.Render("! " + f.Text))
	}
}

func (m *model) addPending(p ws.Pending) {
	for _, q := range m.pending {
		if q.Approval.ID == p.Approval.ID {
			return
		}
	}
	m.pending = append(m.pending, p)
	m.state = "waiting approval"
}

func (m *model) title(text string) {
	if m.summary.Title != "" || strings.TrimSpace(text) == "" {
		return
	}
	r := []rune(strings.Join(strings.Fields(text), " "))
	if len(r) > titleRunes {
		m.summary.Title = string(r[:titleRunes]) + "…"
		return
	}
	m.summary.Title = string(r)
}

func isExit(text string) bool {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "exit", "/exit", "/quit":
		return true
	}
	return false
}

func (m *model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}
	if len(m.pending) > 0 {
		if k.Type == tea.KeyRunes && len(k.Runes) == 1 && (k.Runes[0] == 'y' || k.Runes[0] == 'n') {
			p := m.pending[0]
			m.pending = m.pending[1:]
			approve := k.Runes[0] == 'y'
			verdict := "  ✗ ditolak: "
			if approve {
				verdict = "  ✓ diizinkan: "
			}
			m.state = "running"
			return m, tea.Batch(
				m.print(dimStyle.Render(verdict+p.Approval.Tool+" → "+p.Approval.Target)),
				m.sendCmd(ws.Decide{TaskID: p.TaskID, ApprovalID: p.Approval.ID, Approve: approve}),
			)
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(string(m.input))
		m.input = nil
		switch {
		case text == "":
			return m, nil
		case isExit(text):
			return m, tea.Quit
		}
		m.title(text)
		m.summary.UserMessages++
		m.state = "running"
		return m, tea.Batch(m.print(userLine(text)), m.sendCmd(ws.Input{Conv: m.opts.Conv, Text: text}))
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

func userLine(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n" + userStyle.Render("› "+text)
}

func alfredLine(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return dotStyle.Render("●") + " " + text
}

func (m *model) card(f ws.Frame) string {
	model := f.Model
	if model == "" {
		model = "default"
	}
	if f.DefaultModel != "" {
		model += " · " + f.DefaultModel
	}
	lines := []string{nameStyle.Render("◆ Alfred") + "  " + dimStyle.Render("v"+m.opts.Display), dimStyle.Render(model)}
	if dir := shortPath(m.opts.Workdir, m.opts.Home); dir != "" {
		lines = append(lines, dimStyle.Render(dir))
	}
	return cardStyle.Width(min(max(m.width-2, 30), 60)).Render(strings.Join(lines, "\n"))
}

func shortPath(dir, home string) string {
	if home != "" && (dir == home || strings.HasPrefix(dir, home+"/")) {
		return "~" + strings.TrimPrefix(dir, home)
	}
	return dir
}

func (m *model) View() string {
	w := max(m.width, 20)
	rule := ruleStyle.Render(strings.Repeat("─", w))
	var parts []string
	if len(m.pending) > 0 {
		p := m.pending[0].Approval
		head := warnStyle.Render("Butuh izin") + dimStyle.Render(fmt.Sprintf(" · tier %d", p.Tier))
		if p.Escalated {
			head += dimStyle.Render(" · naik karena konten tidak tepercaya")
		}
		body := []string{head, p.Tool + " → " + p.Target}
		if p.Preview != "" {
			body = append(body, dimStyle.Render(p.Preview))
		}
		body = append(body, okStyle.Render("[y] izinkan")+"   "+dimStyle.Render("[n] tolak"))
		parts = append(parts, askStyle.Width(w-2).Render(strings.Join(body, "\n")))
	}
	prompt := userStyle.Render("› ") + string(m.input)
	switch {
	case len(m.pending) > 0:
		prompt = userStyle.Render("› ") + dimStyle.Render("(menunggu izin…)")
	case len(m.input) == 0:
		prompt = userStyle.Render("› ") + dimStyle.Render("tanya Alfred…")
	}
	parts = append(parts, rule, prompt, rule, dimStyle.Render("  ● "+m.state+" · exit / /exit / /quit untuk keluar"))
	return strings.Join(parts, "\n")
}
