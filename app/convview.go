package app

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/panca/haythos/core/task"
)

const firstUserRunes = 60

type TurnView struct {
	User, Answer string
	At           time.Time
}

type PendingApproval struct {
	TaskID   string
	Approval ApprovalRequest
}

type ConversationView struct {
	Turns   []TurnView
	Pending []PendingApproval
	Model   string
	// UserMessages and ToolCalls count the whole conversation, for the chat's exit summary.
	UserMessages, ToolCalls int
}

type ConversationSummary struct {
	ReplyTo, FirstUser string
	LastAt             time.Time
}

type taskScan struct {
	replyTo   string
	state     task.State
	firstUser string
	request   *approvalRequested
	calls     int
}

// scanLog reads the whole event log once.
// TODO: uncertain — full event-log scan per call, fine for one user; add an index if the log grows past ~1e5 events
func (a *App) scanLog(ctx context.Context) ([]task.Event, map[string]*taskScan, error) {
	events, err := a.d.Events.Since(ctx, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("read event log: %w", err)
	}
	tasks := map[string]*taskScan{}
	for _, e := range events {
		if e.Type == task.EvCreated {
			var c task.Created
			if err := json.Unmarshal(e.Payload, &c); err != nil {
				return nil, nil, fmt.Errorf("decode created event %d: %w", e.Cursor, err)
			}
			tasks[e.TaskID] = &taskScan{replyTo: c.ReplyTo}
			continue
		}
		t := tasks[e.TaskID]
		if t == nil {
			continue
		}
		switch e.Type {
		case task.EvState:
			var c task.StateChange
			if json.Unmarshal(e.Payload, &c) == nil {
				t.state = c.State
			}
		case task.EvMessage:
			var m task.Message
			if json.Unmarshal(e.Payload, &m) != nil {
				continue
			}
			if t.firstUser == "" && m.Role == task.RoleUser && !m.Context {
				t.firstUser = m.Text
			}
			if m.Role == task.RoleAssistant && !m.Context {
				t.calls += len(m.Calls)
			}
		case task.EvApprovalRequested:
			var r approvalRequested
			if json.Unmarshal(e.Payload, &r) == nil {
				t.request = &r
			}
		}
	}
	return events, tasks, nil
}

// ConversationView returns the recorded turns of replyTo and the approvals its tasks still wait on.
func (a *App) ConversationView(ctx context.Context, replyTo string) (ConversationView, error) {
	events, tasks, err := a.scanLog(ctx)
	if err != nil {
		return ConversationView{}, err
	}
	stream := convStream(replyTo)
	model, err := conversationModel(events, stream)
	if err != nil {
		return ConversationView{}, err
	}
	v := ConversationView{Model: model}
	seen := map[string]bool{}
	for _, e := range events {
		switch {
		case e.TaskID == stream && e.Type == evConvTurn:
			var t convTurn
			if err := json.Unmarshal(e.Payload, &t); err != nil {
				return ConversationView{}, fmt.Errorf("decode conversation turn %d: %w", e.Cursor, err)
			}
			if !seen[t.Task] {
				seen[t.Task] = true
				v.Turns = append(v.Turns, TurnView{User: t.User, Answer: t.Answer, At: t.At})
			}
		case e.Type == task.EvCreated:
			t := tasks[e.TaskID]
			if t.replyTo == replyTo && t.firstUser != "" {
				v.UserMessages++
				v.ToolCalls += t.calls
			}
			if t.replyTo == replyTo && t.state == task.WaitingApproval && t.request != nil {
				r := t.request
				v.Pending = append(v.Pending, PendingApproval{TaskID: e.TaskID, Approval: ApprovalRequest{
					Approval: r.Approval, Tier: r.Tier, Escalated: r.Escalated, Preview: r.Preview,
				}})
			}
		}
	}
	return v, nil
}

func conversationModel(events []task.Event, stream string) (string, error) {
	for _, e := range slices.Backward(events) {
		if e.TaskID == stream && e.Type == evConversationModel {
			var model string
			if err := json.Unmarshal(e.Payload, &model); err != nil {
				return "", fmt.Errorf("decode conversation model %d: %w", e.Cursor, err)
			}
			return model, nil
		}
	}
	return "", nil
}

// Conversations lists the conversations whose address starts with prefix, most recently active first.
func (a *App) Conversations(ctx context.Context, prefix string) ([]ConversationSummary, error) {
	events, tasks, err := a.scanLog(ctx)
	if err != nil {
		return nil, err
	}
	type conv struct {
		sum  ConversationSummary
		last int64
	}
	convs := map[string]*conv{}
	touch := func(replyTo string, e task.Event, at time.Time) *conv {
		c := convs[replyTo]
		if c == nil {
			c = &conv{sum: ConversationSummary{ReplyTo: replyTo}}
			convs[replyTo] = c
		}
		c.last = e.Cursor
		if at.After(c.sum.LastAt) {
			c.sum.LastAt = at
		}
		return c
	}
	convPrefix := convStream(prefix)
	for _, e := range events {
		if t := tasks[e.TaskID]; t != nil && t.replyTo != "" && strings.HasPrefix(t.replyTo, prefix) {
			c := touch(t.replyTo, e, e.TS)
			if c.sum.FirstUser == "" {
				c.sum.FirstUser = firstRunes(t.firstUser, firstUserRunes)
			}
			continue
		}
		if e.Type == evConversationModel && strings.HasPrefix(e.TaskID, convPrefix) {
			touch(strings.TrimPrefix(e.TaskID, "conv:"), e, e.TS)
			continue
		}
		if e.Type == evConvTurn && strings.HasPrefix(e.TaskID, convPrefix) {
			var t convTurn
			if err := json.Unmarshal(e.Payload, &t); err != nil {
				return nil, fmt.Errorf("decode conversation turn %d: %w", e.Cursor, err)
			}
			c := touch(strings.TrimPrefix(e.TaskID, "conv:"), e, t.At)
			if c.sum.FirstUser == "" {
				c.sum.FirstUser = firstRunes(t.User, firstUserRunes)
			}
		}
	}
	list := make([]*conv, 0, len(convs))
	for _, c := range convs {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].last > list[j].last })
	out := make([]ConversationSummary, len(list))
	for i, c := range list {
		out[i] = c.sum
	}
	return out, nil
}

// TaskOrigin is the conversation a task was started from; empty for scheduled tasks.
func (a *App) TaskOrigin(ctx context.Context, taskID string) (string, error) {
	events, err := a.d.Events.Load(ctx, taskID)
	if err != nil {
		return "", fmt.Errorf("load task %s: %w", taskID, err)
	}
	for _, e := range events {
		if e.Type == task.EvCreated {
			var c task.Created
			if err := json.Unmarshal(e.Payload, &c); err != nil {
				return "", fmt.Errorf("decode created event of %s: %w", taskID, err)
			}
			return c.ReplyTo, nil
		}
	}
	return "", fmt.Errorf("task %s: not found", taskID)
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
