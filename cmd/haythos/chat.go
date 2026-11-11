package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/adapters/tui"
)

const dialProbe = time.Second

// connect starts the kernel when nobody answers on the socket, then dials it.
func connect(ctx context.Context, home paths, args []string) (*ws.Client, error) {
	probe := func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, dialProbe)
		defer cancel()
		c, err := ws.Dial(ctx, home.Socket)
		if err != nil {
			return err
		}
		return c.Close()
	}
	if err := ensureKernel(ctx, home, func() error { return spawnKernel(home, args) }, probe); err != nil {
		return nil, err
	}
	return ws.Dial(ctx, home.Socket)
}

func runChat(ctx context.Context, home paths, serve []string, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	resume := fs.Bool("resume", false, "continue the latest TUI conversation")
	session := fs.String("session", "", "continue the given TUI conversation")
	model := fs.String("model", "", "select a model for this conversation")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse chat flags: %w", err)
	}
	c, err := connect(ctx, home, serve)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	var items []ws.Session
	if *resume || *session != "" {
		if items, err = listSessions(ctx, c); err != nil {
			return err
		}
	}
	conv, err := pickConv(items, *resume, *session, newConvID)
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	userHome, _ := os.UserHomeDir()
	version := binaryVersion()
	opts := tui.Options{Conv: conv, Version: version, Display: displayOf(version), Workdir: wd, Home: userHome, Model: *model}
	final, err := tea.NewProgram(tui.New(c, opts), tea.WithContext(ctx)).Run()
	if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return fmt.Errorf("run chat: %w", err)
	}
	if err := tui.StartupError(final); err != nil {
		return err
	}
	if s, ok := tui.SummaryOf(final); ok {
		printSummary(os.Stdout, s, time.Now())
	}
	return nil
}

// printSummary is the Hermes-style recap printed after the chat closes.
func printSummary(out io.Writer, s tui.Summary, now time.Time) {
	id := strings.TrimPrefix(s.Conv, ws.Prefix)
	title := s.Title
	if title == "" {
		title = "(belum ada)"
	}
	lines := []string{
		"",
		"Lanjutkan sesi ini dengan:",
		"  haythos chat --session " + id,
		"  haythos chat --resume",
		"",
		fmt.Sprintf("%-10s %s", "Sesi:", s.Conv),
		fmt.Sprintf("%-10s %s", "Judul:", title),
		fmt.Sprintf("%-10s %s", "Durasi:", spoken(now.Sub(s.Started))),
		fmt.Sprintf("%-10s %d (%d kamu, %d tool)", "Pesan:", s.UserMessages+s.ToolCalls, s.UserMessages, s.ToolCalls),
	}
	if s.Pending > 0 {
		lines = append(lines, fmt.Sprintf("%-10s %d izin belum diputuskan", "Menunggu:", s.Pending))
	}
	lines = append(lines, "Alfred tetap jalan di background (Discord dan job terjadwal aktif).")
	_, _ = fmt.Fprintln(out, strings.Join(lines, "\n"))
}

func runSessions(ctx context.Context, home paths, serve []string, out io.Writer) error {
	c, err := connect(ctx, home, serve)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	items, err := listSessions(ctx, c)
	if err != nil {
		return err
	}
	printSessions(out, items, time.Local)
	return nil
}

func listSessions(ctx context.Context, c *ws.Client) ([]ws.Session, error) {
	if err := c.Send(ctx, ws.Sessions{}); err != nil {
		return nil, err
	}
	f, err := c.Next(ctx)
	if err != nil {
		return nil, err
	}
	if f.Type != ws.TypeSessionsList {
		return nil, fmt.Errorf("kernel answered sessions with %q: %s", f.Type, f.Message)
	}
	return f.Items, nil
}

// pickConv: a new conversation by default; items are newest first.
func pickConv(items []ws.Session, resume bool, session string, fresh func() string) (string, error) {
	switch {
	case resume && session != "":
		return "", errors.New("use either --resume or --session, not both")
	case resume:
		if len(items) == 0 {
			return "", errors.New("no TUI conversation to resume; run `haythos chat` to start one")
		}
		return items[0].Conv, nil
	case session != "":
		want := ws.Prefix + strings.TrimPrefix(session, ws.Prefix)
		for _, it := range items {
			if it.Conv == want {
				return want, nil
			}
		}
		return "", fmt.Errorf("unknown session %q; `haythos sessions` lists them", session)
	}
	return fresh(), nil
}

func newConvID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return ws.Prefix + hex.EncodeToString(b)
}

func printSessions(out io.Writer, items []ws.Session, loc *time.Location) {
	for _, it := range items {
		_, _ = fmt.Fprintf(out, "%s  %s  %s\n", it.Conv, it.LastAt.In(loc).Format("2006-01-02 15:04"), tui.Clean(it.FirstUser))
	}
}

// spoken renders 13m51s as "13m 51s".
func spoken(d time.Duration) string {
	d = d.Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}
