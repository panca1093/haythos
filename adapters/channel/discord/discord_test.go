package discord

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
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

func TestOnlyTheOwnerIsAuthorized(t *testing.T) {
	t.Parallel()
	c := &Channel{cfg: Config{OwnerID: "me"}}
	if !c.authorized("me") || c.authorized("other") || c.authorized("") {
		t.Error("authorization wrong")
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   facts
		want decision
	}{
		{name: "non-owner in DM", in: facts{DM: true}},
		{name: "non-owner mentioning in channel", in: facts{Mentioned: true}},
		{name: "non-owner in active thread", in: facts{Thread: true, Active: true}},
		{name: "owner DM", in: facts{Owner: true, DM: true}, want: decision{Handle: true}},
		{name: "owner DM with mention", in: facts{Owner: true, DM: true, Mentioned: true}, want: decision{Handle: true}},
		{name: "channel without mention", in: facts{Owner: true}},
		{name: "channel with mention starts a thread", in: facts{Owner: true, Mentioned: true}, want: decision{Handle: true, StartThread: true, Activate: true}},
		{name: "active thread without mention", in: facts{Owner: true, Thread: true, Active: true}, want: decision{Handle: true}},
		{name: "active thread with mention", in: facts{Owner: true, Thread: true, Active: true, Mentioned: true}, want: decision{Handle: true}},
		{name: "inactive thread without mention", in: facts{Owner: true, Thread: true}},
		{name: "inactive thread with mention joins it", in: facts{Owner: true, Thread: true, Mentioned: true}, want: decision{Handle: true, Activate: true}},
		{name: "mention-only thread without mention", in: facts{Owner: true, Thread: true, Active: true, MentionOnly: true}},
		{name: "mention-only thread with mention", in: facts{Owner: true, Thread: true, Active: true, MentionOnly: true, Mentioned: true}, want: decision{Handle: true}},
		{name: "mention-only flag on an inactive thread, mentioned", in: facts{Owner: true, Thread: true, MentionOnly: true, Mentioned: true}, want: decision{Handle: true, Activate: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decide(tt.in); got != tt.want {
				t.Errorf("decide(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStripMention(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"<@42> check disk", "check disk"},
		{"check <@!42> disk", "check  disk"},
		{"<@42>", ""},
		{"hello <@99> and <@42>", "hello <@99> and"},
		{"no mention", "no mention"},
	}
	for _, tt := range tests {
		if got := strings.TrimSpace(stripMention(tt.in, "42")); got != tt.want {
			t.Errorf("stripMention(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTextCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"/mention-only", mentionOnlyCmd, true},
		{"/pause", "pause", true},
		{"/resume", "resume", true},
		{"/settings", "settings", true},
		{"/jobs", "jobs", true},
		{"/model", "", false},
		{"/mention-only please", "", false},
		{"/Jobs", "", false},
		{"mention-only", "", false},
		{"what does /pause do?", "", false},
	}
	for _, tt := range tests {
		if got, ok := textCommand(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("textCommand(%q) = %q, %v, want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestThreadName(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"check the disk", "check the disk"},
		{"line one\nline two", "line one line two"},
		{"   ", "Alfred"},
		{strings.Repeat("é", 150), strings.Repeat("é", 99) + "…"},
	}
	for _, tt := range tests {
		got := threadName(tt.in)
		if got != tt.want || utf8.RuneCountInString(got) > 100 {
			t.Errorf("threadName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	t.Parallel()
	chans := map[string]*discordgo.Channel{
		"text":     {ID: "text", GuildID: "g", Type: discordgo.ChannelTypeGuildText},
		"news":     {ID: "news", GuildID: "g", Type: discordgo.ChannelTypeGuildNews},
		"thread":   {ID: "thread", GuildID: "g", Type: discordgo.ChannelTypeGuildPublicThread},
		"private":  {ID: "private", GuildID: "g", Type: discordgo.ChannelTypeGuildPrivateThread},
		"voice":    {ID: "voice", GuildID: "g", Type: discordgo.ChannelTypeGuildVoice},
		"category": {ID: "category", GuildID: "g", Type: discordgo.ChannelTypeGuildCategory},
		"otherdm":  {ID: "otherdm", Type: discordgo.ChannelTypeDM},
	}
	lookup := func(id string) (*discordgo.Channel, error) {
		if ch, ok := chans[id]; ok {
			return ch, nil
		}
		return nil, errors.New("unknown channel")
	}
	tests := []struct {
		name, raw, dm string
		want          string
		wantNote      bool
	}{
		{name: "empty goes to DM", raw: "", dm: "dm-me", want: "dm-me"},
		{name: "literal dm", raw: "dm", dm: "dm-me", want: "dm-me"},
		{name: "the DM channel id", raw: "dm-me", dm: "dm-me", want: "dm-me"},
		{name: "text channel", raw: "text", dm: "dm-me", want: "text"},
		{name: "news channel", raw: "news", dm: "dm-me", want: "news"},
		{name: "public thread", raw: "thread", dm: "dm-me", want: "thread"},
		{name: "private thread", raw: "private", dm: "dm-me", want: "private"},
		{name: "voice channel falls back", raw: "voice", dm: "dm-me", want: "dm-me", wantNote: true},
		{name: "category falls back", raw: "category", dm: "dm-me", want: "dm-me", wantNote: true},
		{name: "someone else's DM falls back", raw: "otherdm", dm: "dm-me", want: "dm-me", wantNote: true},
		{name: "unknown id falls back", raw: "999", dm: "dm-me", want: "dm-me", wantNote: true},
		{name: "unknown DM and bad target", raw: "999", dm: "", want: "", wantNote: true},
		{name: "no DM known and dm requested", raw: "dm", dm: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, note := resolveTarget(tt.raw, tt.dm, lookup)
			if got != tt.want || (note != "") != tt.wantNote {
				t.Errorf("resolveTarget(%q) = %q, %q; want %q, note=%v", tt.raw, got, note, tt.want, tt.wantNote)
			}
		})
	}
	_, note := resolveTarget("bad`target", "dm-me", lookup)
	if strings.Count(note, "`") != 2 {
		t.Errorf("note %q must not let the raw target break out of its code span", note)
	}
}

func TestMentionOnlyReply(t *testing.T) {
	t.Parallel()
	on := mentionOnlyReply(app.ThreadMode{Active: true, MentionOnly: true})
	off := mentionOnlyReply(app.ThreadMode{Active: true})
	notJoined := mentionOnlyReply(app.ThreadMode{})
	if !strings.Contains(on, "only when you @mention") || !strings.Contains(off, "every message") || !strings.Contains(notJoined, "not joined") {
		t.Errorf("replies = %q / %q / %q", on, off, notJoined)
	}
}

func TestChannelEntries(t *testing.T) {
	t.Parallel()
	got := channelEntries("Home", []*discordgo.Channel{
		{ID: "1", Name: "infra", Type: discordgo.ChannelTypeGuildText},
		{ID: "2", Name: "voice", Type: discordgo.ChannelTypeGuildVoice},
		{ID: "3", Name: "news", Type: discordgo.ChannelTypeGuildNews},
	}, []*discordgo.Channel{
		{ID: "4", Name: "disk alerts", Type: discordgo.ChannelTypeGuildPublicThread},
	})
	want := []channelEntry{
		{ID: "1", Name: "infra", Guild: "Home", Kind: "channel"},
		{ID: "3", Name: "news", Guild: "Home", Kind: "channel"},
		{ID: "4", Name: "disk alerts", Guild: "Home", Kind: "thread"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("entries = %+v, want %+v", got, want)
	}
}

func TestRenderNeverPings(t *testing.T) {
	t.Parallel()
	for _, n := range []app.Notice{
		{Kind: app.NoticeMessage, Text: "@everyone"},
		{Kind: app.NoticeApproval, Approval: &app.ApprovalRequest{Preview: "x", Approval: policy.Approval{ID: "a"}}},
	} {
		for _, m := range render(n) {
			if m.AllowedMentions == nil || len(m.AllowedMentions.Parse) != 0 || len(m.AllowedMentions.Users) != 0 {
				t.Errorf("%s message allows mentions: %+v", n.Kind, m.AllowedMentions)
			}
		}
	}
}

func TestKeepTypingRepeatsUntilStopped(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	sent := 0
	stop := keepTyping(5*time.Millisecond, func() {
		mu.Lock()
		sent++
		mu.Unlock()
	})
	time.Sleep(30 * time.Millisecond)
	stop()
	mu.Lock()
	atStop := sent
	mu.Unlock()
	if atStop < 2 {
		t.Fatalf("sent %d typing signals, want it repeated", atStop)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if sent != atStop {
		t.Errorf("typing continued after stop: %d -> %d", atStop, sent)
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
		{Name: "disk", Source: "chat", Cron: "CRON_TZ=Asia/Jakarta 0 9 * * 1", Enabled: false, DeliverTo: []string{"123", "dm-me"}},
	}, "dm-me")
	want := "**Jobs**\n" +
		"`vm-health` (config) `0 7,15 * * *` next Mon 02 Nov 09:00 WIB · tools: vmhealth.read → DM\n" +
		"`disk` (chat) `0 9 * * 1` disabled · tools: none → <#123>, DM\n" +
		"Create, change, disable or delete chat jobs by asking Alfred; each change asks for approval."
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestToolActivityIsNotRenderedInDiscord(t *testing.T) {
	if got := Render(app.Notice{Kind: app.NoticeTool, Text: "fs.read README.md"}); len(got) != 0 {
		t.Fatalf("render = %+v, want nothing for tool activity", got)
	}
}
