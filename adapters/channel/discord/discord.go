package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

const (
	maxMessage = 1900
	idPrefix   = "ap"
	handleTO   = 30 * time.Minute

	modelSelectID = "mdl"
	modelTO       = 30 * time.Second
	maxMenuItems  = 25
	maxFieldLen   = 100

	// Discord shows a typing signal for about 10 seconds.
	typingEvery = 8 * time.Second

	threadNameMax    = 100
	threadArchiveMin = 1440
	mentionOnlyCmd   = "mention-only"
	channelsToolName = "discord.channels"
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
	ActivateThread(ctx context.Context, id string) error
	SetThreadMentionOnly(ctx context.Context, id string, on bool) error
	ThreadModes(ctx context.Context) (map[string]app.ThreadMode, error)
}

type Config struct {
	Token   string
	OwnerID string
}

type Channel struct {
	cfg Config
	h   Handler
	s   *discordgo.Session
	dm  string

	mu    sync.Mutex
	modes map[string]app.ThreadMode
}

// facts are everything the trigger rule needs to know about one incoming message.
type facts struct {
	Owner, DM, Thread, Mentioned, Active, MentionOnly bool
}

type decision struct {
	Handle, StartThread, Activate bool
}

// decide is the whole trigger policy: DMs always, channels only on a mention (in a new thread), threads once joined.
func decide(f facts) decision {
	switch {
	case !f.Owner:
		return decision{}
	case f.DM:
		return decision{Handle: true}
	case f.Thread && f.Active:
		return decision{Handle: !f.MentionOnly || f.Mentioned}
	case f.Thread:
		return decision{Handle: f.Mentioned, Activate: f.Mentioned}
	case f.Mentioned:
		return decision{Handle: true, StartThread: true, Activate: true}
	}
	return decision{}
}

func stripMention(text, botID string) string {
	return strings.NewReplacer("<@"+botID+">", "", "<@!"+botID+">", "").Replace(text)
}

func threadName(text string) string {
	name := strings.Join(strings.Fields(text), " ")
	if name == "" {
		return "Alfred"
	}
	return truncate(name, threadNameMax)
}

func listable(ch *discordgo.Channel) bool { return ch.GuildID != "" && messageable(ch.Type) }

func messageable(t discordgo.ChannelType) bool {
	switch t {
	case discordgo.ChannelTypeGuildText, discordgo.ChannelTypeGuildNews,
		discordgo.ChannelTypeGuildPublicThread, discordgo.ChannelTypeGuildPrivateThread, discordgo.ChannelTypeGuildNewsThread:
		return true
	}
	return false
}

// resolveTarget maps a delivery address to a channel ID; anything the bot cannot see in a guild goes to the owner DM with a note.
func resolveTarget(raw, dm string, lookup func(id string) (*discordgo.Channel, error)) (id, note string) {
	if raw == "" || raw == "dm" || raw == dm {
		return dm, ""
	}
	if ch, err := lookup(raw); err == nil && listable(ch) {
		return raw, ""
	}
	return dm, fmt.Sprintf("Delivery to `%s` failed; sending here instead.\n", truncate(strings.ReplaceAll(raw, "`", ""), 40))
}

func mentionOnlyReply(m app.ThreadMode) string {
	switch {
	case m.MentionOnly:
		return "Mention-only is on: in this thread I answer only when you @mention me."
	case m.Active:
		return "Mention-only is off: I answer every message you send in this thread."
	}
	return "Mention-only is off, but I have not joined this thread yet: @mention me once and I will answer every message after that."
}

type channelEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Guild string `json:"guild"`
	Kind  string `json:"kind"`
}

func channelEntries(guild string, channels, threads []*discordgo.Channel) []channelEntry {
	var out []channelEntry
	for _, group := range []struct {
		kind string
		list []*discordgo.Channel
	}{{"channel", channels}, {"thread", threads}} {
		for _, ch := range group.list {
			if messageable(ch.Type) {
				out = append(out, channelEntry{ID: ch.ID, Name: ch.Name, Guild: guild, Kind: group.kind})
			}
		}
	}
	return out
}

func New(cfg Config) (*Channel, error) {
	s, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("create discord session: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentMessageContent
	c := &Channel{cfg: cfg, s: s, modes: map[string]app.ThreadMode{}}
	s.AddHandler(c.onMessage)
	s.AddHandler(c.onInteraction)
	return c, nil
}

// SetHandler must be called before Open; the app needs the channel as its Notifier, so it is wired second.
func (c *Channel) SetHandler(h Handler) { c.h = h }

func (c *Channel) Open() error {
	// Resolved before the gateway opens so handlers never race on c.dm.
	dm, err := c.s.UserChannelCreate(c.cfg.OwnerID)
	if err != nil {
		return fmt.Errorf("open owner DM: %w", err)
	}
	c.dm = dm.ID
	ctx, cancel := context.WithTimeout(context.Background(), modelTO)
	defer cancel()
	modes, err := c.h.ThreadModes(ctx)
	if err != nil {
		return fmt.Errorf("load thread modes: %w", err)
	}
	c.modes = modes
	if err := c.s.Open(); err != nil {
		return fmt.Errorf("open discord gateway: %w", err)
	}
	cmds := []*discordgo.ApplicationCommand{
		{Name: "pause", Description: "Stop all writes immediately"},
		{Name: "resume", Description: "Allow writes again"},
		{Name: "model", Description: "Show or switch the model"},
		{Name: "settings", Description: "Show runtime settings and what changed them"},
		{Name: "jobs", Description: "List scheduled jobs and their next run"},
		{Name: mentionOnlyCmd, Description: "In a thread: toggle answering only when mentioned"},
	}
	if _, err := c.s.ApplicationCommandBulkOverwrite(c.s.State.User.ID, "", cmds); err != nil {
		return fmt.Errorf("register slash commands: %w", err)
	}
	return nil
}

func (c *Channel) Close() error { return c.s.Close() }

func (c *Channel) authorized(userID string) bool { return userID == c.cfg.OwnerID }

func (c *Channel) channel(id string) (*discordgo.Channel, error) {
	if ch, err := c.s.State.Channel(id); err == nil {
		return ch, nil
	}
	ch, err := c.s.Channel(id)
	if err != nil {
		return nil, fmt.Errorf("get channel %s: %w", id, err)
	}
	return ch, nil
}

func (c *Channel) mode(id string) app.ThreadMode {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modes[id]
}

func (c *Channel) activate(ctx context.Context, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.modes[id].Active {
		return
	}
	if err := c.h.ActivateThread(ctx, id); err != nil {
		slog.Error("activate thread failed", "err", err)
		return
	}
	m := c.modes[id]
	m.Active = true
	c.modes[id] = m
}

func (c *Channel) toggleMentionOnly(ctx context.Context, id string) (app.ThreadMode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.modes[id]
	m.MentionOnly = !m.MentionOnly
	if err := c.h.SetThreadMentionOnly(ctx, id, m.MentionOnly); err != nil {
		return app.ThreadMode{}, err
	}
	c.modes[id] = m
	return m, nil
}

// Publish never logs message content, which may carry secrets.
func (c *Channel) Publish(ctx context.Context, n app.Notice) error {
	to, note := resolveTarget(n.ReplyTo, c.dm, c.channel)
	if to == "" {
		return errors.New("send discord message: owner DM unavailable")
	}
	msgs := render(n)
	msgs[0].Content = note + msgs[0].Content
	for _, m := range msgs {
		if _, err := c.s.ChannelMessageSendComplex(to, m, discordgo.WithContext(ctx)); err != nil {
			return fmt.Errorf("send discord message: %w", err)
		}
	}
	return nil
}

func render(n app.Notice) []*discordgo.MessageSend {
	msgs := renderNotice(n)
	for _, m := range msgs {
		m.AllowedMentions = &discordgo.MessageAllowedMentions{}
	}
	return msgs
}

func renderNotice(n app.Notice) []*discordgo.MessageSend {
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

// keepTyping sends now and every interval; the returned stop waits so nothing is sent after it.
func keepTyping(every time.Duration, send func()) (stop func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			send()
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() {
		close(done)
		<-exited
	}
}

func (c *Channel) typing(channelID string) (stop func()) {
	return keepTyping(typingEvery, func() {
		if err := c.s.ChannelTyping(channelID); err != nil {
			slog.Debug("typing signal failed", "err", err)
		}
	})
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
	if m.Author == nil || m.Author.Bot || !c.authorized(m.Author.ID) {
		return
	}
	botID := s.State.User.ID
	text := strings.TrimSpace(stripMention(m.Content, botID))
	if text == "" {
		return
	}
	f := facts{Owner: true, DM: m.GuildID == "", Mentioned: slices.ContainsFunc(m.Mentions, func(u *discordgo.User) bool { return u.ID == botID })}
	if !f.DM {
		if ch, err := c.channel(m.ChannelID); err != nil {
			slog.Warn("channel lookup failed; treating as a plain channel", "err", err)
		} else if ch.IsThread() {
			md := c.mode(m.ChannelID)
			f.Thread, f.Active, f.MentionOnly = true, md.Active, md.MentionOnly
		}
	}
	d := decide(f)
	if !d.Handle {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), handleTO)
		defer cancel()
		replyTo := m.ChannelID
		if d.StartThread {
			th, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{Name: threadName(text), AutoArchiveDuration: threadArchiveMin}, discordgo.WithContext(ctx))
			if err != nil {
				slog.Error("start thread failed", "err", err)
				_ = c.Publish(ctx, app.Notice{Kind: app.NoticeAlert, Text: "I could not start a thread for that message: " + err.Error(), ReplyTo: m.ChannelID})
				return
			}
			replyTo = th.ID
		}
		if d.Activate {
			c.activate(ctx, replyTo)
		}
		stop := c.typing(replyTo)
		defer stop()
		if _, err := c.h.HandleInput(ctx, app.Input{Text: text, ReplyTo: replyTo}); err != nil {
			slog.Error("handle input failed", "err", err)
			_ = c.Publish(ctx, app.Notice{Kind: app.NoticeAlert, Text: "I could not process that message: " + err.Error(), ReplyTo: replyTo})
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
	if !c.authorized(userID) {
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
		stop := c.typing(i.ChannelID)
		defer stop()
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
	switch i.ApplicationCommandData().Name {
	case "model":
		c.onModelCommand(s, i)
		return
	case mentionOnlyCmd:
		c.onMentionOnly(s, i)
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
			msg = jobsText(js, c.dm)
		}
	default:
		return
	}
	c.respond(s, i, discordgo.InteractionResponseChannelMessageWithSource, clip(msg, maxMessage), nil)
}

func (c *Channel) onMentionOnly(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	msg := "This only works inside a thread."
	if ch, err := c.channel(i.ChannelID); err == nil && ch.IsThread() {
		if m, err := c.toggleMentionOnly(ctx, i.ChannelID); err != nil {
			msg = "Could not change mention-only: " + err.Error()
		} else {
			msg = mentionOnlyReply(m)
		}
	}
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: msg, Flags: discordgo.MessageFlagsEphemeral},
	})
	if err != nil {
		slog.Error("interaction respond failed", "err", err)
	}
}

// ChannelsTool is the discord.channels tool, so the model can turn "#infra" into an ID for deliver_to.
func (c *Channel) ChannelsTool() (task.ToolSpec, func(ctx context.Context, args []byte) (task.ToolResult, error)) {
	spec := task.ToolSpec{
		Name:        channelsToolName,
		Description: "List the Discord text channels and active threads the bot can see, with id, name and guild. Use the id in jobs.define deliver_to.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Untrusted:   true,
	}
	return spec, func(ctx context.Context, _ []byte) (task.ToolResult, error) {
		entries, err := c.listChannels(ctx)
		if err != nil {
			return errResult(err), nil
		}
		b, err := json.Marshal(entries)
		if err != nil {
			return task.ToolResult{}, fmt.Errorf("encode channels: %w", err)
		}
		return task.ToolResult{Content: string(b), Untrusted: true}, nil
	}
}

func errResult(err error) task.ToolResult {
	return task.ToolResult{Content: err.Error(), IsError: true}
}

func (c *Channel) listChannels(ctx context.Context) ([]channelEntry, error) {
	opt := discordgo.WithContext(ctx)
	guilds, err := c.s.UserGuilds(200, "", "", false, opt)
	if err != nil {
		return nil, fmt.Errorf("list guilds: %w", err)
	}
	out := []channelEntry{}
	for _, g := range guilds {
		chans, err := c.s.GuildChannels(g.ID, opt)
		if err != nil {
			return nil, fmt.Errorf("list channels of %s: %w", g.Name, err)
		}
		active, err := c.s.GuildThreadsActive(g.ID, opt)
		if err != nil {
			return nil, fmt.Errorf("list threads of %s: %w", g.Name, err)
		}
		out = append(out, channelEntries(g.Name, chans, active.Threads)...)
	}
	return out, nil
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

func jobsText(js []app.JobStatus, dm string) string {
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
		lines = append(lines, fmt.Sprintf("`%s` (%s) `%s` %s · tools: %s → %s", j.Name, j.Source, strings.TrimPrefix(j.Cron, "CRON_TZ=Asia/Jakarta "), when, tools, targetsText(j.DeliverTo, dm)))
	}
	lines = append(lines, "Create, change, disable or delete chat jobs by asking Alfred; each change asks for approval.")
	return strings.Join(lines, "\n")
}

func targetsText(targets []string, dm string) string {
	if len(targets) == 0 {
		return "DM"
	}
	parts := make([]string, len(targets))
	for i, t := range targets {
		parts[i] = "<#" + t + ">"
		if t == "dm" || t == dm {
			parts[i] = "DM"
		}
	}
	return strings.Join(parts, ", ")
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
