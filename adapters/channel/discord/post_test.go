package discord

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestParsePost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    postArgs
		wantErr bool
	}{
		{name: "valid", in: `{"channel":"123","text":"hi"}`, want: postArgs{Channel: "123", Text: "hi"}},
		{name: "channel trimmed", in: `{"channel":" 123 ","text":"hi"}`, want: postArgs{Channel: "123", Text: "hi"}},
		{name: "dm", in: `{"channel":"dm","text":"hi"}`, want: postArgs{Channel: "dm", Text: "hi"}},
		{name: "missing channel", in: `{"text":"hi"}`, wantErr: true},
		{name: "blank channel", in: `{"channel":"  ","text":"hi"}`, wantErr: true},
		{name: "missing text", in: `{"channel":"123"}`, wantErr: true},
		{name: "blank text", in: `{"channel":"123","text":" \n "}`, wantErr: true},
		{name: "not json", in: `nope`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePost([]byte(tt.in))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("parsePost(%s) = %+v, %v; want %+v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestPostTarget(t *testing.T) {
	t.Parallel()
	lookup := func(id string) (*discordgo.Channel, error) {
		switch id {
		case "text":
			return &discordgo.Channel{ID: id, GuildID: "g", Type: discordgo.ChannelTypeGuildText}, nil
		case "news":
			return &discordgo.Channel{ID: id, GuildID: "g", Type: discordgo.ChannelTypeGuildNews}, nil
		case "thread":
			return &discordgo.Channel{ID: id, GuildID: "g", Type: discordgo.ChannelTypeGuildPublicThread}, nil
		case "voice":
			return &discordgo.Channel{ID: id, GuildID: "g", Type: discordgo.ChannelTypeGuildVoice}, nil
		case "other-dm":
			return &discordgo.Channel{ID: id, Type: discordgo.ChannelTypeDM}, nil
		}
		return nil, errors.New("unknown channel")
	}
	tests := []struct {
		raw, want string
		wantErr   bool
	}{
		{raw: "text", want: "text"},
		{raw: "news", want: "news"},
		{raw: "thread", want: "thread"},
		{raw: "dm", want: "dm-me"},
		{raw: "dm-me", want: "dm-me"},
		{raw: "voice", wantErr: true},
		{raw: "other-dm", wantErr: true},
		{raw: "999", wantErr: true},
		{raw: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Parallel()
			got, err := postTarget(tt.raw, "dm-me", lookup)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("postTarget(%q) = %q, %v; want %q, err=%v", tt.raw, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type sent struct {
	channel string
	msg     *discordgo.MessageSend
}

func okLookup(id string) (*discordgo.Channel, error) {
	return &discordgo.Channel{ID: id, GuildID: "g", Type: discordgo.ChannelTypeGuildText}, nil
}

func TestPostChunksAndSuppressesMentions(t *testing.T) {
	t.Parallel()
	var got []sent
	send := func(_ context.Context, ch string, m *discordgo.MessageSend) error {
		got = append(got, sent{ch, m})
		return nil
	}
	text := strings.Repeat("a", maxMessage) + "@everyone"
	if err := post(context.Background(), postArgs{Channel: "c1", Text: text}, "dm-me", okLookup, send); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].channel != "c1" || got[1].channel != "c1" {
		t.Fatalf("sent = %+v, want 2 chunks to c1", got)
	}
	if got[0].msg.Content+got[1].msg.Content != text {
		t.Error("chunks do not reassemble to the text")
	}
	for _, s := range got {
		if s.msg.AllowedMentions == nil || len(s.msg.AllowedMentions.Parse) != 0 || len(s.msg.AllowedMentions.Users) != 0 || len(s.msg.AllowedMentions.Roles) != 0 {
			t.Errorf("AllowedMentions = %+v, want empty", s.msg.AllowedMentions)
		}
	}
}

func TestPostRejectedTargetSendsNothing(t *testing.T) {
	t.Parallel()
	called := false
	send := func(context.Context, string, *discordgo.MessageSend) error { called = true; return nil }
	lookup := func(string) (*discordgo.Channel, error) { return nil, errors.New("unknown") }
	if err := post(context.Background(), postArgs{Channel: "999", Text: "hi"}, "dm-me", lookup, send); err == nil {
		t.Fatal("want error for unknown channel")
	}
	if called {
		t.Error("nothing may be sent to a rejected target")
	}
}

func TestPostDMUsesOwnerChannel(t *testing.T) {
	t.Parallel()
	var to string
	send := func(_ context.Context, ch string, _ *discordgo.MessageSend) error { to = ch; return nil }
	if err := post(context.Background(), postArgs{Channel: "dm", Text: "hi"}, "dm-me", okLookup, send); err != nil {
		t.Fatal(err)
	}
	if to != "dm-me" {
		t.Errorf("sent to %q, want dm-me", to)
	}
}

func TestPostSendFailureIsReturned(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	send := func(context.Context, string, *discordgo.MessageSend) error { return boom }
	if err := post(context.Background(), postArgs{Channel: "c1", Text: "hi"}, "dm-me", okLookup, send); !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapped boom", err)
	}
}
