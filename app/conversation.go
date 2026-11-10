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
	evConvTurn          = task.EventType("conv.turn")
	evConvSummary       = task.EventType("conv.summary")
	extractWindowTokens = 7000
	convDMLookback      = 24 * time.Hour
	convTailShare       = 0.10
	convSummaryFraming  = "Summary of the earlier conversation (reference only; if it conflicts with the latest user message, the latest message wins):\n"
	convSummaryRequest  = "Summarize the conversation below for your own later reference. Use these sections: Goal; Constraints & preferences; Decisions; Done; Open questions & pending; Key facts (names, paths, numbers, links). Keep facts, drop pleasantries. If an earlier summary is given, update it with the new turns instead of rewriting from scratch. At most about 1,500 words. Output only the summary."
)

type convTurn struct {
	Task    string    `json:"task"`
	User    string    `json:"user"`
	Answer  string    `json:"answer"`
	At      time.Time `json:"at"`
	Context int       `json:"context,omitempty"`
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

// splitTail is splitWindow that always keeps the newest turn.
func splitTail(turns []convTurn, budget int) (older, recent []convTurn) {
	older, recent = splitWindow(turns, budget)
	if len(recent) == 0 && len(turns) > 0 {
		return turns[:len(turns)-1], turns[len(turns)-1:]
	}
	return older, recent
}

func estimatePrompt(summary string, fresh []convTurn, incoming string) int {
	est := (len(summary) + len(incoming)) / 4
	for _, t := range fresh {
		est += t.tokens()
	}
	if n := len(fresh); n > 0 && fresh[n-1].Context > 0 {
		est = max(est, fresh[n-1].Context+len(incoming)/4)
	}
	return est
}

// fitTurn cuts the answer so the turn fits budget tokens; the user text is kept whole.
func fitTurn(t convTurn, budget int) (convTurn, bool) {
	const marker = "\n[truncated]"
	if t.tokens() <= budget {
		return t, false
	}
	keep := min(max(budget*4-len(t.User)-len(marker), 0), len(t.Answer))
	t.Answer = strings.ToValidUTF8(t.Answer[:keep], "") + marker
	return t, true
}

func (a *App) chatWindow() int {
	model := a.d.DefaultJob.Model
	if sel := a.d.ModelSelector; sel != nil && a.d.Windows[sel.Current()] > 0 {
		model = sel.Current()
	}
	return a.window(model)
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
	if s.FirstInput > 0 {
		turn.Context = s.FirstInput + len(answer)/4
	}
	marker := brainTurn{ReplyTo: s.Created.ReplyTo, Task: id, At: turn.At, Tainted: s.Tainted}
	if _, err := a.d.Events.Append(ctx, brainStream, evBrainTurn, marker); err != nil {
		return fmt.Errorf("record brain turn marker: %w", err)
	}
	if _, err := a.d.Events.Append(ctx, convStream(s.Created.ReplyTo), evConvTurn, turn); err != nil {
		return fmt.Errorf("record conversation turn: %w", err)
	}
	return nil
}

// conversation returns the earlier turns of replyTo as context messages: a summary of folded turns, then recent turns verbatim.
func (a *App) conversation(ctx context.Context, replyTo string, dm bool, incoming string) ([]task.Message, error) {
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
	window := a.chatWindow()
	recent := fresh
	limit := a.d.CompactAt * float64(window)
	if float64(estimatePrompt(summary.Text, fresh, incoming)) >= limit {
		var older []convTurn
		older, recent = splitTail(fresh, int(convTailShare*float64(window)))
		if len(older) > 0 {
			if summary, err = a.compact(ctx, stream, summary, older, replyTo); err != nil {
				return nil, err
			}
		}
		if len(recent) == 1 {
			fit, cut := fitTurn(recent[0], int(limit)-(len(summary.Text)+len(incoming))/4)
			if cut {
				slog.Warn("newest conversation turn alone exceeds the compaction threshold; truncating its answer in the replay", "conversation", replyTo, "tokens", recent[0].tokens())
				recent = []convTurn{fit}
			}
		}
	}
	var out []task.Message
	if summary.Text != "" {
		out = append(out, task.Message{Role: task.RoleSystem, Text: convSummaryFraming + summary.Text, Context: true, Kind: task.KindSummary})
	}
	for _, t := range recent {
		out = append(out,
			task.Message{Role: task.RoleUser, Text: t.User, Context: true},
			task.Message{Role: task.RoleAssistant, Text: t.Answer, Context: true})
	}
	return out, nil
}

func (a *App) compact(ctx context.Context, stream string, summary convSummary, older []convTurn, replyTo string) (convSummary, error) {
	text, err := a.summarize(ctx, summary.Text, older)
	if err != nil {
		slog.Warn("summarize conversation failed; replaying the tail only for this message", "conversation", replyTo, "err", err)
		return summary, nil
	}
	next := convSummary{Text: text, Covers: append(append([]string{}, summary.Covers...), turnIDs(older)...)}
	if _, err := a.d.Events.Append(ctx, stream, evConvSummary, next); err != nil {
		return summary, fmt.Errorf("record conversation summary: %w", err)
	}
	return next, nil
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
