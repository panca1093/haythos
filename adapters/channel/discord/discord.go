package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/panca/haythos/app"
)

const (
	maxMessage = 1900
	idPrefix   = "ap"
	handleTO   = 30 * time.Minute
)

type Handler interface {
	HandleInput(ctx context.Context, in app.Input) (string, error)
	HandleApproval(ctx context.Context, taskID, approvalID string, approve bool) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
}

type Config struct {
	Token     string
	OwnerID   string
	ChannelID string
}

type Channel struct {
	cfg Config
	h   Handler
	s   *discordgo.Session
}

func New(cfg Config) (*Channel, error) {
	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent
	c := &Channel{cfg: cfg, s: s}
	s.AddHandler(c.onMessage)
	s.AddHandler(c.onInteraction)
	return c, nil
}

// SetHandler must be called before Open; the app needs the channel as its Notifier, so it is wired second.
func (c *Channel) SetHandler(h Handler) { c.h = h }

func (c *Channel) Open() error {
	if err := c.s.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	cmds := []*discordgo.ApplicationCommand{
		{Name: "pause", Description: "Stop all writes immediately"},
		{Name: "resume", Description: "Allow writes again"},
	}
	if _, err := c.s.ApplicationCommandBulkOverwrite(c.s.State.User.ID, "", cmds); err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}
	return nil
}

func (c *Channel) Close() error { return c.s.Close() }

func (c *Channel) authorized(userID, channelID string) bool {
	return userID == c.cfg.OwnerID && (channelID == c.cfg.ChannelID || c.cfg.ChannelID == "")
}

// Publish never logs message content, which may carry secrets.
func (c *Channel) Publish(ctx context.Context, n app.Notice) error {
	for _, m := range render(n) {
		if _, err := c.s.ChannelMessageSendComplex(c.cfg.ChannelID, m, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send discord message: %w", err)
		}
	}
	return nil
}

func render(n app.Notice) []*discordgo.MessageSend {
	switch n.Kind {
	case app.NoticeApproval:
		a := n.Approval
		text := fmt.Sprintf("**Approval needed** (tier %d)\n```\n%s\n```", a.Tier, clip(a.Preview, 1500))
		if a.Escalated {
			text += "\nEscalated: the target came from untrusted content, not from your instructions."
		}
		text += fmt.Sprintf("\nExpires <t:%d:R>", a.Approval.Expiry.Unix())
		return []*discordgo.MessageSend{{
			Content: text,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "Approve", Style: discordgo.SuccessButton, CustomID: customID(n.TaskID, a.Approval.ID, true)},
				discordgo.Button{Label: "Reject", Style: discordgo.DangerButton, CustomID: customID(n.TaskID, a.Approval.ID, false)},
			}}},
		}}
	case app.NoticeAlert:
		return chunks("🚨 " + n.Text)
	case app.NoticeState:
		return chunks("⚠️ " + n.Text)
	default:
		return chunks(n.Text)
	}
}

func chunks(s string) []*discordgo.MessageSend {
	var out []*discordgo.MessageSend
	for len(s) > maxMessage {
		out = append(out, &discordgo.MessageSend{Content: s[:maxMessage]})
		s = s[maxMessage:]
	}
	return append(out, &discordgo.MessageSend{Content: s})
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func customID(taskID, approvalID string, approve bool) string {
	v := "n"
	if approve {
		v = "y"
	}
	return strings.Join([]string{idPrefix, taskID, approvalID, v}, "|")
}

func parseCustomID(id string) (taskID, approvalID string, approve, ok bool) {
	p := strings.Split(id, "|")
	if len(p) != 4 || p[0] != idPrefix || (p[3] != "y" && p[3] != "n") {
		return "", "", false, false
	}
	return p[1], p[2], p[3] == "y", true
}

func (c *Channel) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || !c.authorized(m.Author.ID, m.ChannelID) || strings.TrimSpace(m.Content) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), handleTO)
		defer cancel()
		if _, err := c.h.HandleInput(ctx, app.Input{Text: m.Content}); err != nil {
			slog.Error("handle input failed", "err", err)
			_ = c.Publish(ctx, app.Notice{Kind: app.NoticeAlert, Text: "I could not process that message: " + err.Error()})
		}
	}()
}

func (c *Channel) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := ""
	if i.Member != nil && i.Member.User != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}
	if !c.authorized(userID, i.ChannelID) {
		c.respond(s, i, discordgo.InteractionResponseChannelMessageWithSource, "Not authorized.", nil)
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		c.onCommand(s, i)
	case discordgo.InteractionMessageComponent:
		taskID, approvalID, approve, ok := parseCustomID(i.MessageComponentData().CustomID)
		if !ok {
			return
		}
		verdict := "Rejected"
		if approve {
			verdict = "Approved"
		}
		c.respond(s, i, discordgo.InteractionResponseUpdateMessage, i.Message.Content+"\n**"+verdict+"**", []discordgo.MessageComponent{})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), handleTO)
			defer cancel()
			if err := c.h.HandleApproval(ctx, taskID, approvalID, approve); err != nil {
				slog.Error("handle approval failed", "err", err)
				_ = c.Publish(ctx, app.Notice{Kind: app.NoticeAlert, Text: "Approval failed: " + err.Error()})
			}
		}()
	}
}

func (c *Channel) onCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var msg string
	switch i.ApplicationCommandData().Name {
	case "pause":
		if err := c.h.Pause(ctx); err != nil {
			msg = "Pause failed: " + err.Error()
		} else {
			msg = "Paused. All writes are stopped."
		}
	case "resume":
		if err := c.h.Resume(ctx); err != nil {
			msg = "Resume failed: " + err.Error()
		} else {
			msg = "Resumed. Writes still need approval."
		}
	default:
		return
	}
	c.respond(s, i, discordgo.InteractionResponseChannelMessageWithSource, msg, nil)
}

func (c *Channel) respond(s *discordgo.Session, i *discordgo.InteractionCreate, typ discordgo.InteractionResponseType, content string, comps []discordgo.MessageComponent) {
	data := &discordgo.InteractionResponseData{Content: content}
	if comps != nil {
		data.Components = comps
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: typ, Data: data}); err != nil {
		slog.Error("interaction respond failed", "err", err)
	}
}
