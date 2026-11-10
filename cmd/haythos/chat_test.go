package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/channel/ws"
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
