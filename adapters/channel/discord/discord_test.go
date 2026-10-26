package discord

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
)

func TestCustomIDRoundTrip(t *testing.T) {
	task, ap, ok, valid := parseCustomID(customID("vm-health-17", "ap-c1", true))
	if !valid || !ok || task != "vm-health-17" || ap != "ap-c1" {
		t.Errorf("got %q %q %v %v", task, ap, ok, valid)
	}
	if _, _, approve, valid := parseCustomID(customID("t", "a", false)); !valid || approve {
		t.Error("reject not parsed")
	}
	for _, bad := range []string{"", "ap|t|a", "xx|t|a|y", "ap|t|a|maybe"} {
		if _, _, _, valid := parseCustomID(bad); valid {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if len(customID(strings.Repeat("t", 40), strings.Repeat("a", 30), true)) > 100 {
		t.Error("custom id over Discord's 100 char limit")
	}
}

func TestAuthorizedOnlyOwnerInConfiguredChannel(t *testing.T) {
	c := &Channel{cfg: Config{OwnerID: "me", ChannelID: "ch"}}
	if !c.authorized("me", "ch") || c.authorized("other", "ch") || c.authorized("me", "elsewhere") {
		t.Error("authorization wrong")
	}
}

func TestRenderApprovalHasButtonsAndEscalationNote(t *testing.T) {
	n := app.Notice{Kind: app.NoticeApproval, TaskID: "T", Approval: &app.ApprovalRequest{
		Tier: policy.TierHighImpact, Escalated: true, Preview: "slack.post {...}",
		Approval: policy.Approval{ID: "ap-1", Expiry: time.Unix(1800000000, 0)},
	}}
	msgs := render(n)
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "untrusted") || !strings.Contains(msgs[0].Content, "tier 3") {
		t.Fatalf("content = %q", msgs[0].Content)
	}
	row := msgs[0].Components[0].(discordgo.ActionsRow)
	if len(row.Components) != 2 {
		t.Errorf("buttons = %d", len(row.Components))
	}
}

func TestLongMessagesAreChunked(t *testing.T) {
	msgs := render(app.Notice{Kind: app.NoticeMessage, Text: strings.Repeat("x", maxMessage*2+10)})
	if len(msgs) != 3 {
		t.Errorf("chunks = %d, want 3", len(msgs))
	}
}
