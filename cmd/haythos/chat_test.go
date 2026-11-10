package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
	"github.com/panca/haythos/adapters/tui"
)

func TestPickConv(t *testing.T) {
	t.Parallel()
	items := []ws.Session{{Conv: "tui:new000000001"}, {Conv: "tui:old000000002"}}
	tests := []struct {
		name    string
		items   []ws.Session
		resume  bool
		session string
		want    string
		wantErr string
	}{
		{"fresh", items, false, "", "fresh", ""},
		{"resume takes the newest", items, true, "", "tui:new000000001", ""},
		{"resume with none", nil, true, "", "", "no TUI conversation"},
		{"session by id", items, false, "old000000002", "tui:old000000002", ""},
		{"session with prefix", items, false, "tui:old000000002", "tui:old000000002", ""},
		{"unknown session", items, false, "nope", "", "unknown session"},
		{"both flags", items, true, "old000000002", "", "either"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := pickConv(tt.items, tt.resume, tt.session, func() string { return "fresh" })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestNewConvID(t *testing.T) {
	t.Parallel()
	a, b := newConvID(), newConvID()
	if !regexp.MustCompile(`^tui:[0-9a-f]{12}$`).MatchString(a) || a == b {
		t.Fatalf("ids %q %q, want distinct tui:<12 hex>", a, b)
	}
}

func TestPrintSessions(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	at := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	printSessions(&out, []ws.Session{{Conv: "tui:abc", FirstUser: "check the vm", LastAt: at}}, time.UTC)
	if got, want := out.String(), "tui:abc  2026-11-10 09:30  check the vm\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPrintSummary(t *testing.T) {
	var b strings.Builder
	start := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	printSummary(&b, tui.Summary{Conv: "tui:f6d6cc7554ab", Title: "Cek README", Started: start, UserMessages: 3, ToolCalls: 2, Pending: 1}, start.Add(13*time.Minute+51*time.Second))
	out := b.String()
	for _, want := range []string{
		"haythos chat --session f6d6cc7554ab", "haythos chat --resume",
		"Sesi:", "tui:f6d6cc7554ab", "Judul:", "Cek README", "Durasi:", "13m 51s",
		"Pesan:", "5 (3 kamu, 2 tool)", "Menunggu:", "1 izin", "tetap jalan",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	printSummary(&b, tui.Summary{Conv: "tui:x", Started: start}, start.Add(4*time.Second))
	if strings.Contains(b.String(), "Menunggu") || !strings.Contains(b.String(), "(belum ada)") || !strings.Contains(b.String(), "4s") {
		t.Errorf("empty session summary:\n%s", b.String())
	}
}

func TestDisplayVersionHidesBuildDetails(t *testing.T) {
	for in, want := range map[string]string{"6682326-dirty+35fe48e9": "6682326", "6682326+35fe48e9": "6682326", "dev": "dev"} {
		if got := displayOf(in); got != want {
			t.Errorf("displayOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPrintSessionsStripsControlCharacters(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printSessions(&out, []ws.Session{{Conv: "tui:abc", FirstUser: "hi \x1b]52;c;cGF3bmVk\x07there", LastAt: time.Unix(0, 0)}}, time.UTC)
	if strings.ContainsRune(out.String(), 0x1b) || strings.ContainsRune(out.String(), 0x07) {
		t.Fatalf("session list kept control characters: %q", out.String())
	}
}
