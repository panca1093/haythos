package discord

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"github.com/panca/haythos/app"
)

func TestUndoIDRoundTrip(t *testing.T) {
	tests := []struct {
		id   string
		item string
		ok   bool
	}{
		{undoID("42"), "42", true},
		{"un|", "", false},
		{"ap|t|a|y", "", false},
		{"unknown", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			item, ok := parseUndoID(tt.id)
			if item != tt.item || ok != tt.ok {
				t.Errorf("parseUndoID(%q) = %q, %v", tt.id, item, ok)
			}
		})
	}
	if classify(undoID("7")) != kindUndo {
		t.Error("undo button not classified as undo")
	}
}

func TestDigestRendersOneUndoButtonPerItemAndRemovalKeepsTheRest(t *testing.T) {
	var actions []app.UndoAction
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		actions = append(actions, app.UndoAction{ID: id, Label: "inbox/item-" + id})
	}
	msgs := renderNotice(app.Notice{Kind: app.NoticeDigest, Text: "digest", Undo: actions})
	rows := msgs[len(msgs)-1].Components
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (5 buttons per row)", len(rows))
	}
	if rest := withoutButton(rows, undoID("6")); len(rest) != 1 {
		t.Fatalf("rows after removing the only button of row 2 = %d, want 1", len(rest))
	}
	rest := withoutButton(rows, undoID("1"))
	if got := len(rest[0].(discordgo.ActionsRow).Components); got != 4 {
		t.Errorf("row 1 buttons after removal = %d, want 4", got)
	}
}

func TestTaintedNoticeSuppressesEmbedsOnEveryChunk(t *testing.T) {
	for _, tainted := range []bool{true, false} {
		for _, m := range render(app.Notice{Kind: app.NoticeMessage, Text: strings.Repeat("x", maxMessage+1), Tainted: tainted}) {
			if got := m.Flags&discordgo.MessageFlagsSuppressEmbeds != 0; got != tainted {
				t.Errorf("tainted=%v suppress=%v", tainted, got)
			}
		}
	}
}
