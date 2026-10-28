package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/panca/haythos/core/task"
)

const (
	evConvTurn         = task.EventType("conv.turn")
	evConvSummary      = task.EventType("conv.summary")
	convWindowTokens   = 7000
	convDMLookback     = 24 * time.Hour
	convSummaryRequest = "Summarize the conversation below for your own later reference. Keep facts, decisions, names and open questions; drop pleasantries. At most 200 words. Output only the summary."
)

type convTurn struct {
	Task   string    `json:"task"`
	User   string    `json:"user"`
	Answer string    `json:"answer"`
	At     time.Time `json:"at"`
}

type convSummary struct {
	Text   string   `json:"text"`
	Covers []string `json:"covers"`
}

func convStream(replyTo string) string { return "conv:" + replyTo }

func (t convTurn) tokens() int { return (len(t.User) + len(t.Answer)) / 4 }

// splitWindow keeps the newest turns that fit the token budget; older turns overflow.
func splitWindow(turns []convTurn, budget int) (older, recent []convTurn) {
	used, i := 0, len(turns)
	for i > 0 {
		used += turns[i-1].tokens()
		if used > budget {
			break
		}
		i--
	}
	return turns[:i], turns[i:]
}

// recordTurn runs before the task turns Done so a crash cannot lose it; readers drop duplicates by task id.
func (a *App) recordTurn(ctx context.Context, id, answer string) error {
	s, err := a.Snapshot(ctx, id)
	if err != nil {
		return err
	}
	if s.Created.ReplyTo == "" {
		return nil
	}
	var user string
	for _, m := range s.History {
		if m.Role == task.RoleUser && !m.Context {
			user = m.Text
			break
		}
	}
	turn := convTurn{Task: id, User: user, Answer: answer, At: a.d.Clock()}
	if _, err := a.d.Events.Append(ctx, convStream(s.Created.ReplyTo), evConvTurn, turn); err != nil {
		return fmt.Errorf("record conversation turn: %w", err)
	}
	return nil
}

// conversation returns the earlier turns of replyTo as context messages: a summary of folded turns, then recent turns verbatim.
func (a *App) conversation(ctx context.Context, replyTo string, dm bool) ([]task.Message, error) {
	if replyTo == "" {
		return nil, nil
	}
	stream := convStream(replyTo)
	unlock := a.lock(stream)
	defer unlock()
	events, err := a.d.Events.Load(ctx, stream)
	if err != nil {
		return nil, fmt.Errorf("load conversation %s: %w", replyTo, err)
	}
	var turns []convTurn
	var summary convSummary
	seen := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case evConvTurn:
			var t convTurn
			if err := json.Unmarshal(e.Payload, &t); err != nil {
				return nil, fmt.Errorf("decode conversation turn %d: %w", e.Cursor, err)
			}
			if !seen[t.Task] && (!dm || !t.At.Before(a.d.Clock().Add(-convDMLookback))) {
				seen[t.Task] = true
				turns = append(turns, t)
			}
		case evConvSummary:
			if err := json.Unmarshal(e.Payload, &summary); err != nil {
				return nil, fmt.Errorf("decode conversation summary %d: %w", e.Cursor, err)
			}
		}
	}
	covered := map[string]bool{}
	for _, id := range summary.Covers {
		covered[id] = true
	}
	for _, id := range summary.Covers {
		if !seen[id] {
			summary, covered = convSummary{}, map[string]bool{}
			break
		}
	}
	var fresh []convTurn
	for _, t := range turns {
		if !covered[t.Task] {
			fresh = append(fresh, t)
		}
	}
	older, recent := splitWindow(fresh, convWindowTokens)
	if len(older) > 0 {
		text, err := a.summarize(ctx, summary.Text, older)
		if err != nil {
			slog.Warn("summarize conversation failed; dropping folded turns for this message", "conversation", replyTo, "err", err)
		} else {
			summary = convSummary{Text: text, Covers: append(summary.Covers, turnIDs(older)...)}
			if _, err := a.d.Events.Append(ctx, stream, evConvSummary, summary); err != nil {
				return nil, fmt.Errorf("record conversation summary: %w", err)
			}
		}
	}
	var out []task.Message
	if summary.Text != "" {
		out = append(out, task.Message{Role: task.RoleSystem, Text: "Summary of the earlier conversation:\n" + summary.Text, Context: true})
	}
	for _, t := range recent {
		out = append(out,
			task.Message{Role: task.RoleUser, Text: t.User, Context: true},
			task.Message{Role: task.RoleAssistant, Text: t.Answer, Context: true})
	}
	return out, nil
}

func turnIDs(turns []convTurn) []string {
	ids := make([]string, len(turns))
	for i, t := range turns {
		ids[i] = t.Task
	}
	return ids
}

func (a *App) summarize(ctx context.Context, previous string, turns []convTurn) (string, error) {
	model, ok := a.d.Models[a.d.DefaultJob.Model]
	if !ok {
		return "", fmt.Errorf("model %q is not configured", a.d.DefaultJob.Model)
	}
	var b strings.Builder
	if previous != "" {
		b.WriteString("Earlier summary:\n" + previous + "\n\n")
	}
	b.WriteString("New turns:\n")
	for _, t := range turns {
		fmt.Fprintf(&b, "User: %s\nAlfred: %s\n\n", t.User, t.Answer)
	}
	stepCtx, cancel := context.WithTimeout(ctx, a.d.StepTimeout)
	defer cancel()
	turn, err := model.Step(stepCtx, []task.Message{
		{Role: task.RoleSystem, Text: convSummaryRequest},
		{Role: task.RoleUser, Text: b.String()},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("summarize turns: %w", err)
	}
	text := strings.TrimSpace(turn.Text)
	if text == "" {
		return "", fmt.Errorf("summarize turns: empty summary")
	}
	return text, nil
}
