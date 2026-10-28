package discord

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestComponentKind(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want componentKind
	}{
		{"approve button", customID("t", "a", true), kindApproval},
		{"reject button", customID("t", "a", false), kindApproval},
		{"model select", modelSelectID, kindModel},
		{"empty", "", kindUnknown},
		{"approval-shaped but bad verdict", "ap|t|a|maybe", kindUnknown},
		{"model id with suffix", modelSelectID + "|x", kindUnknown},
		{"task id equal to model id", customID(modelSelectID, "a", true), kindApproval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.id); got != tt.want {
				t.Errorf("classify(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
	if _, _, _, ok := parseCustomID(modelSelectID); ok {
		t.Error("approval parser accepted the model select id")
	}
}

func menuOptions(t *testing.T, comps []discordgo.MessageComponent) []discordgo.SelectMenuOption {
	t.Helper()
	if len(comps) != 1 {
		t.Fatalf("rows = %d", len(comps))
	}
	row := comps[0].(discordgo.ActionsRow)
	menu := row.Components[0].(discordgo.SelectMenu)
	if menu.CustomID != modelSelectID || menu.MenuType != discordgo.StringSelectMenu {
		t.Errorf("menu = %+v", menu)
	}
	return menu.Options
}

func TestModelMenu(t *testing.T) {
	many := make([]app.ModelChoice, 30)
	for i := range many {
		many[i] = app.ModelChoice{Value: fmt.Sprintf("m%d", i), Name: "M"}
	}
	long := strings.Repeat("x", 300)
	tests := []struct {
		name        string
		current     string
		choices     []app.ModelChoice
		wantOpts    int
		wantDefault string
		wantContent string
	}{
		{"current is marked default", "sonnet", []app.ModelChoice{{Value: "default", Name: "Default"}, {Value: "sonnet", Name: "Sonnet", Description: "fast"}}, 2, "sonnet", "`sonnet`"},
		{"capped at 25", "m0", many, 25, "m0", "`m0`"},
		{"over-long fields are clipped", "", []app.ModelChoice{{Value: "a", Name: long, Description: long}}, 1, "", "not set"},
		{"empty name falls back to value", "", []app.ModelChoice{{Value: "a"}}, 1, "", ""},
		{"empty and oversized values are skipped", "", []app.ModelChoice{{Value: ""}, {Value: long}, {Value: "ok", Name: "OK"}}, 1, "", ""},
		{"no choices means no menu", "x", nil, 0, "", "`x`"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, comps := modelMenu(tt.current, tt.choices)
			if !strings.Contains(content, tt.wantContent) {
				t.Errorf("content = %q, want to contain %q", content, tt.wantContent)
			}
			if tt.wantOpts == 0 {
				if len(comps) != 0 {
					t.Errorf("components = %d, want none", len(comps))
				}
				return
			}
			opts := menuOptions(t, comps)
			if len(opts) != tt.wantOpts {
				t.Fatalf("options = %d, want %d", len(opts), tt.wantOpts)
			}
			def := ""
			for _, o := range opts {
				if n := utf8.RuneCountInString; n(o.Label) == 0 || n(o.Label) > 100 || len(o.Value) == 0 || len(o.Value) > 100 || n(o.Description) > 100 {
					t.Errorf("option violates Discord limits: %+v", o)
				}
				if o.Default {
					def = o.Value
				}
			}
			if def != tt.wantDefault {
				t.Errorf("default = %q, want %q", def, tt.wantDefault)
			}
		})
	}
}

func TestSettingsText(t *testing.T) {
	got := settingsText([]app.SettingValue{
		{Key: "model", Value: "sonnet", Default: "default", Overridden: true},
		{Key: "quiet", Value: "off", Default: "off"},
	})
	want := "**Settings**\n`model` = `sonnet` (changed; default `default`)\n`quiet` = `off` (default)"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got := settingsText(nil); got != "No runtime settings." {
		t.Errorf("empty = %q", got)
	}
}

func TestJobsText(t *testing.T) {
	next := time.Date(2026, 11, 2, 2, 0, 0, 0, time.UTC)
	got := jobsText([]app.JobStatus{
		{Name: "vm-health", Source: "config", Cron: "CRON_TZ=Asia/Jakarta 0 7,15 * * *", Tools: []string{"vmhealth.read"}, Enabled: true, Next: next},
		{Name: "disk", Source: "chat", Cron: "CRON_TZ=Asia/Jakarta 0 9 * * 1", Enabled: false},
	})
	want := "**Jobs**\n" +
		"`vm-health` (config) `0 7,15 * * *` next Mon 02 Nov 09:00 WIB · tools: vmhealth.read\n" +
		"`disk` (chat) `0 9 * * 1` disabled · tools: none\n" +
		"Create, change, disable or delete chat jobs by asking Alfred; each change asks for approval."
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
