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

	modelSelectID = "mdl"
	modelTO       = 30 * time.Second
	maxMenuItems  = 25
	maxFieldLen   = 100
)

type componentKind int

const (
	kindUnknown componentKind = iota
	kindApproval
	kindModel
)

type Handler interface {
	HandleInput(ctx context.Context, in app.Input) (string, error)
	HandleApproval(ctx context.Context, taskID, approvalID string, approve bool) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	ListModels(ctx context.Context) ([]app.ModelChoice, error)
	CurrentModel() string
	SelectModel(ctx context.Context, value string) error
	Settings(ctx context.Context) ([]app.SettingValue, error)
	ListJobs(ctx context.Context) ([]app.JobStatus, error)
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
	dm  string
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
	// Resolved before the gateway opens so handlers never race on c.dm.
	if dm, err := c.s.UserChannelCreate(c.cfg.OwnerID); err != nil {
		slog.Warn("owner DM unavailable; replying in the channel only", "err", err)
	} else {
		c.dm = dm.ID
	}
	if err := c.s.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	cmds := []*discordgo.ApplicationCommand{
		{Name: "pause", Description: "Stop all writes immediately"},
		{Name: "resume", Description: "Allow writes again"},
		{Name: "model", Description: "Show or switch the model"},
		{Name: "settings", Description: "Show runtime settings and what changed them"},
		{Name: "jobs", Description: "List scheduled jobs and their next run"},
	}
	if _, err := c.s.ApplicationCommandBulkOverwrite(c.s.State.User.ID, "", cmds); err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}
	return nil
}

func (c *Channel) Close() error { return c.s.Close() }

func (c *Channel) authorized(userID, channelID string) bool {
	return userID == c.cfg.OwnerID && (channelID == c.cfg.ChannelID || c.cfg.ChannelID == "" || (c.dm != "" && channelID == c.dm))
}

func (c *Channel) target(replyTo string) string {
	if replyTo != "" && (replyTo == c.cfg.ChannelID || replyTo == c.dm) {
		return replyTo
	}
	return c.cfg.ChannelID
}

// Publish never logs message content, which may carry secrets.
func (c *Channel) Publish(ctx context.Context, n app.Notice) error {
	for _, m := range render(n) {
		if _, err := c.s.ChannelMessageSendComplex(c.target(n.ReplyTo), m, discordgo.WithContext(ctx)); err != nil {
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

func classify(id string) componentKind {
	if id == modelSelectID {
		return kindModel
	}
	if _, _, _, ok := parseCustomID(id); ok {
		return kindApproval
	}
	return kindUnknown
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func modelMenu(current string, choices []app.ModelChoice) (string, []discordgo.MessageComponent) {
	cur := "not set"
	if current != "" {
		cur = "`" + current + "`"
	}
	content := "Current model: " + cur
	var opts []discordgo.SelectMenuOption
	for _, ch := range choices {
		if ch.Value == "" || len(ch.Value) > maxFieldLen || len(opts) == maxMenuItems {
			continue
		}
		name := ch.Name
		if name == "" {
			name = ch.Value
		}
		opts = append(opts, discordgo.SelectMenuOption{
			Label:       truncate(name, maxFieldLen),
			Value:       ch.Value,
			Description: truncate(ch.Description, maxFieldLen),
			Default:     ch.Value == current,
		})
	}
	if len(opts) == 0 {
		return content + "\nNo models available.", []discordgo.MessageComponent{}
	}
	return content, []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: modelSelectID, Placeholder: "Switch model", MaxValues: 1, Options: opts},
	}}}
}

func (c *Channel) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || !c.authorized(m.Author.ID, m.ChannelID) || strings.TrimSpace(m.Content) == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), handleTO)
		defer cancel()
		if _, err := c.h.HandleInput(ctx, app.Input{Text: m.Content, ReplyTo: m.ChannelID}); err != nil {
			slog.Error("handle input failed", "err", err)
			_ = c.Publish(ctx, app.Notice{Kind: app.NoticeAlert, Text: "I could not process that message: " + err.Error(), ReplyTo: m.ChannelID})
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
		switch classify(i.MessageComponentData().CustomID) {
		case kindApproval:
			c.onApproval(s, i)
		case kindModel:
			c.onModelSelect(s, i)
		}
	}
}

func (c *Channel) onApproval(s *discordgo.Session, i *discordgo.InteractionCreate) {
	taskID, approvalID, approve, _ := parseCustomID(i.MessageComponentData().CustomID)
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
			_ = c.Publish(ctx, app.Notice{Kind: app.NoticeAlert, Text: "Approval failed: " + err.Error(), ReplyTo: i.ChannelID})
		}
	}()
}

func (c *Channel) onModelCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		slog.Error("interaction respond failed", "err", err)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), modelTO)
		defer cancel()
		choices, err := c.h.ListModels(ctx)
		if err != nil {
			c.edit(s, i, "Could not list models: "+err.Error(), []discordgo.MessageComponent{})
			return
		}
		content, comps := modelMenu(c.h.CurrentModel(), choices)
		c.edit(s, i, content, comps)
	}()
}

func (c *Channel) onModelSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate})
	if err != nil {
		slog.Error("interaction respond failed", "err", err)
		return
	}
	values := i.MessageComponentData().Values
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), modelTO)
		defer cancel()
		if len(values) != 1 {
			c.edit(s, i, "No model selected.", []discordgo.MessageComponent{})
			return
		}
		if err := c.h.SelectModel(ctx, values[0]); err != nil {
			c.edit(s, i, "Model switch failed: "+err.Error(), []discordgo.MessageComponent{})
			return
		}
		c.edit(s, i, "Model switched to `"+values[0]+"`. New steps use it.", []discordgo.MessageComponent{})
	}()
}

func (c *Channel) edit(s *discordgo.Session, i *discordgo.InteractionCreate, content string, comps []discordgo.MessageComponent) {
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &content, Components: &comps}); err != nil {
		slog.Error("interaction edit failed", "err", err)
	}
}

func (c *Channel) onCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.ApplicationCommandData().Name == "model" {
		c.onModelCommand(s, i)
		return
	}
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
	case "settings":
		if vals, err := c.h.Settings(ctx); err != nil {
			msg = "Could not read settings: " + err.Error()
		} else {
			msg = settingsText(vals)
		}
	case "jobs":
		if js, err := c.h.ListJobs(ctx); err != nil {
			msg = "Could not list jobs: " + err.Error()
		} else {
			msg = jobsText(js)
		}
	default:
		return
	}
	c.respond(s, i, discordgo.InteractionResponseChannelMessageWithSource, clip(msg, maxMessage), nil)
}

func settingsText(vals []app.SettingValue) string {
	if len(vals) == 0 {
		return "No runtime settings."
	}
	lines := []string{"**Settings**"}
	for _, v := range vals {
		src := "(default)"
		if v.Overridden {
			src = "(changed; default `" + v.Default + "`)"
		}
		lines = append(lines, fmt.Sprintf("`%s` = `%s` %s", v.Key, v.Value, src))
	}
	return strings.Join(lines, "\n")
}

var wib = time.FixedZone("WIB", 7*3600)

func jobsText(js []app.JobStatus) string {
	lines := []string{"**Jobs**"}
	for _, j := range js {
		when := "disabled"
		switch {
		case j.Enabled && j.Next.IsZero():
			when = "manual only"
		case j.Enabled:
			when = "next " + j.Next.In(wib).Format("Mon 02 Jan 15:04") + " WIB"
		}
		tools := "none"
		if len(j.Tools) > 0 {
			tools = strings.Join(j.Tools, ", ")
		}
		lines = append(lines, fmt.Sprintf("`%s` (%s) `%s` %s · tools: %s", j.Name, j.Source, strings.TrimPrefix(j.Cron, "CRON_TZ=Asia/Jakarta "), when, tools))
	}
	lines = append(lines, "Create, change, disable or delete chat jobs by asking Alfred; each change asks for approval.")
	return strings.Join(lines, "\n")
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
