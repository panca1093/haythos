package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
)

const postToolName = "discord.post"

type postArgs struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

func parsePost(args []byte) (postArgs, error) {
	var a postArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return postArgs{}, fmt.Errorf("decode args: %w", err)
	}
	a.Channel = strings.TrimSpace(a.Channel)
	if a.Channel == "" {
		return postArgs{}, errors.New("channel is required: a channel or thread id, or dm")
	}
	if strings.TrimSpace(a.Text) == "" {
		return postArgs{}, errors.New("text is required")
	}
	return a, nil
}

// postTarget accepts only the owner DM or a guild channel or thread the bot can see; unlike delivery there is no DM fallback.
func postTarget(raw, dm string, lookup func(id string) (*discordgo.Channel, error)) (string, error) {
	if raw == "dm" || (raw != "" && raw == dm) {
		return dm, nil
	}
	ch, err := lookup(raw)
	if err != nil {
		return "", fmt.Errorf("channel %s is not visible to the bot: %w", truncate(raw, 40), err)
	}
	if !listable(ch) {
		return "", fmt.Errorf("channel %s is not a server text channel or thread", truncate(raw, 40))
	}
	return raw, nil
}

func post(ctx context.Context, a postArgs, dm string, lookup func(id string) (*discordgo.Channel, error), send func(ctx context.Context, channelID string, m *discordgo.MessageSend) error) error {
	to, err := postTarget(a.Channel, dm, lookup)
	if err != nil {
		return err
	}
	for _, m := range render(app.Notice{Text: a.Text}) {
		if err := send(ctx, to, m); err != nil {
			return fmt.Errorf("send discord message: %w", err)
		}
	}
	return nil
}

// PostTool is the discord.post write tool. discordgo v0.29.0 has no message nonce, so a crashed post is never verifiable and the ledger row ends as outcome_unknown.
func (c *Channel) PostTool() (task.ToolSpec, func(ctx context.Context, args []byte) (task.ToolResult, error)) {
	spec := task.ToolSpec{
		Name:        postToolName,
		Description: "Post a message to a Discord server channel or thread (id from discord.channels) or to the owner's DM (channel \"dm\"). Always needs the owner's approval. Mentions in the text do not ping anyone.",
		Write:       true,
		TargetArg:   "channel",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"channel":{"type":"string","description":"channel or thread id, or dm"},"text":{"type":"string"}},"required":["channel","text"]}`),
	}
	return spec, func(ctx context.Context, args []byte) (task.ToolResult, error) {
		a, err := parsePost(args)
		if err != nil {
			return errResult(err), nil
		}
		send := func(ctx context.Context, id string, m *discordgo.MessageSend) error {
			_, err := c.s.ChannelMessageSendComplex(id, m, discordgo.WithContext(ctx))
			return err
		}
		if err := post(ctx, a, c.dm, c.channel, send); err != nil {
			return errResult(err), nil
		}
		return task.ToolResult{Content: "posted to " + a.Channel}, nil
	}
}
