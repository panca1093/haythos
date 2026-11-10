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
	cursor  int
	width   int
	// modelName, tokens and budget feed the status line.
	modelName              string
	tokens, budget, cached int
	printed                []string
	carded                 bool
	summary                Summary
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
	curStyle  = lipgloss.NewStyle().Reverse(true)
	userBox   = lipgloss.NewStyle().Background(lipgloss.Color("#26333d")).Foreground(lipgloss.Color("#cfe0f5"))
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
	f = cleanFrame(f)
	switch f.Type {
	case ws.TypeWelcome:
		var cmds []tea.Cmd
		m.modelName = modelLabel(f)
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
			cmds = append(cmds, m.print(m.userLine(t.User)), m.print(m.alfredLine(t.Answer)))
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
		if f.Budget > 0 {
			m.tokens, m.budget, m.cached = f.Tokens, f.Budget, f.Cached
		}
		return m.print(m.alfredLine(f.Text))
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
	if m.edit(k) {
		return m, nil
	}
	switch k.Type {
	case tea.KeyEnter:
		text := strings.TrimSpace(string(m.input))
		m.input, m.cursor = nil, 0
		switch {
		case text == "":
			return m, nil
		case isExit(text):
			return m, tea.Quit
		}
		m.title(text)
		m.summary.UserMessages++
		m.state = "running"
		return m, tea.Batch(m.print(m.userLine(text)), m.sendCmd(ws.Input{Conv: m.opts.Conv, Text: text}))
	}
	return m, nil
}

// edit applies line-editing keys the way a shell prompt does; Cmd+Backspace arrives as Ctrl+U.
func (m *model) edit(k tea.KeyMsg) bool {
	in, c := m.input, m.cursor
	switch {
	case k.Type == tea.KeyRunes && k.Alt && len(k.Runes) == 1 && k.Runes[0] == 'b', k.Type == tea.KeyLeft && k.Alt, k.Type == tea.KeyCtrlLeft:
		m.cursor = wordStart(in, c)
	case k.Type == tea.KeyRunes && k.Alt && len(k.Runes) == 1 && k.Runes[0] == 'f', k.Type == tea.KeyRight && k.Alt, k.Type == tea.KeyCtrlRight:
		m.cursor = wordEnd(in, c)
	case k.Type == tea.KeyRunes:
		m.insert(k.Runes)
	case k.Type == tea.KeySpace:
		m.insert([]rune{' '})
	case k.Type == tea.KeyLeft:
		m.cursor = max(0, c-1)
	case k.Type == tea.KeyRight:
		m.cursor = min(len(in), c+1)
	case k.Type == tea.KeyHome, k.Type == tea.KeyCtrlA:
		m.cursor = 0
	case k.Type == tea.KeyEnd, k.Type == tea.KeyCtrlE:
		m.cursor = len(in)
	case k.Type == tea.KeyBackspace && k.Alt, k.Type == tea.KeyCtrlW:
		m.cut(wordStart(in, c), c)
	case k.Type == tea.KeyBackspace:
		m.cut(max(0, c-1), c)
	case k.Type == tea.KeyDelete:
		m.cut(c, min(len(in), c+1))
	case k.Type == tea.KeyCtrlU:
		m.cut(0, c)
	case k.Type == tea.KeyCtrlK:
		m.cut(c, len(in))
	default:
		return false
	}
	return true
}

func (m *model) insert(r []rune) {
	m.input = append(m.input[:m.cursor], append(r, m.input[m.cursor:]...)...)
	m.cursor += len(r)
}

func (m *model) cut(from, to int) {
	m.input = append(m.input[:from:from], m.input[to:]...)
	m.cursor = from
}

func wordStart(in []rune, c int) int {
	for c > 0 && in[c-1] == ' ' {
		c--
	}
	for c > 0 && in[c-1] != ' ' {
		c--
	}
	return c
}

func wordEnd(in []rune, c int) int {
	for c < len(in) && in[c] == ' ' {
		c++
	}
	for c < len(in) && in[c] != ' ' {
		c++
	}
	return c
}

func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r < 0x7f) || r > 0x9f {
			return r
		}
		return -1
	}, s)
}

func cleanApproval(a ws.Approval) ws.Approval {
	a.Tool, a.Target, a.Preview = clean(a.Tool), clean(a.Target), clean(a.Preview)
	return a
}

// cleanFrame strips terminal control sequences from text a tool or file may have shaped.
func cleanFrame(f ws.Frame) ws.Frame {
	f.Text, f.Message = clean(f.Text), clean(f.Message)
	if f.Approval != nil {
		a := cleanApproval(*f.Approval)
		f.Approval = &a
	}
	f.Turns = append([]ws.Turn(nil), f.Turns...)
	for i := range f.Turns {
		f.Turns[i].User, f.Turns[i].Answer = clean(f.Turns[i].User), clean(f.Turns[i].Answer)
	}
	f.Pending = append([]ws.Pending(nil), f.Pending...)
	for i := range f.Pending {
		f.Pending[i].Approval = cleanApproval(f.Pending[i].Approval)
	}
	return f
}

// userLine sets the owner's message apart on a shaded band, after a blank line.
func (m *model) userLine(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n" + userBox.Width(max(m.width, 20)).Render("› "+clean(text))
}

// alfredLine starts after a blank line; wrapped lines hang under the text, not under the dot.
func (m *model) alfredLine(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	body := lipgloss.NewStyle().Width(max(m.width-2, 18)).Render(text)
	return "\n" + dotStyle.Render("●") + " " + strings.ReplaceAll(body, "\n", "\n  ")
}

func modelLabel(f ws.Frame) string {
	label := f.Model
	if label == "" {
		label = "default"
	}
	if f.DefaultModel != "" {
		label += " · " + f.DefaultModel
	}
	return label
}

func (m *model) card(f ws.Frame) string {
	lines := []string{nameStyle.Render("◆ Alfred") + "  " + dimStyle.Render("v"+m.opts.Display), dimStyle.Render(modelLabel(f))}
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
	prompt := userStyle.Render("› ") + m.inputView()
	if len(m.pending) > 0 {
		prompt = userStyle.Render("› ") + dimStyle.Render("(menunggu izin…)")
	}
	parts = append(parts, rule, prompt, rule, m.status())
	return strings.Join(parts, "\n")
}

// inputView draws the text with a block cursor; an empty input shows the cursor over the placeholder.
func (m *model) inputView() string {
	if len(m.input) == 0 {
		ph := []rune("tanya Alfred…")
		return curStyle.Render(string(ph[0])) + dimStyle.Render(string(ph[1:]))
	}
	before, at, after := string(m.input[:m.cursor]), " ", ""
	if m.cursor < len(m.input) {
		at, after = string(m.input[m.cursor]), string(m.input[m.cursor+1:])
	}
	return before + curStyle.Render(at) + after
}

func (m *model) status() string {
	parts := []string{"● " + m.state}
	if m.modelName != "" {
		parts = append(parts, nameStyle.Render(m.modelName))
	}
	ctx := "ctx --"
	if m.budget > 0 {
		ctx = fmt.Sprintf("ctx %s/%s %s", kilo(m.tokens), kilo(m.budget), bar(m.tokens, m.budget, 10))
	}
	parts = append(parts, ctx)
	if m.cached > 0 {
		parts = append(parts, "cache "+kilo(m.cached))
	}
	return dimStyle.Render("  ") + strings.Join(parts, dimStyle.Render(" · "))
}

func kilo(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	s := fmt.Sprintf("%.1f", float64(n)/1000)
	return strings.TrimSuffix(s, ".0") + "k"
}

func bar(used, total, cells int) string {
	filled := min(cells, max(0, (used*cells+total/2)/total))
	return strings.Repeat("▰", filled) + strings.Repeat("▱", cells-filled)
}
