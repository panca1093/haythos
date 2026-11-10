package task

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestTransitionTable(t *testing.T) {
	valid := map[[2]State]bool{
		{Pending, Running}:         true,
		{Running, Done}:            true,
		{Running, Failed}:          true,
		{Running, WaitingApproval}: true,
		{Running, Watching}:        true,
		{WaitingApproval, Running}: true,
		{Watching, Running}:        true,
	}
	all := []State{Pending, Running, WaitingApproval, Watching, Done, Failed}
	for _, from := range all {
		for _, to := range all {
			t.Run(string(from)+"->"+string(to), func(t *testing.T) {
				t.Parallel()
				err := Transition(from, to)
				if valid[[2]State{from, to}] {
					if err != nil {
						t.Errorf("want ok, got %v", err)
					}
				} else if !errors.Is(err, ErrInvalidTransition) {
					t.Errorf("want ErrInvalidTransition, got %v", err)
				}
			})
		}
	}
}

func ev(seq int, typ EventType, payload any) Event {
	b, _ := json.Marshal(payload)
	return Event{Seq: seq, Type: typ, Payload: b}
}

func TestReplay(t *testing.T) {
	events := []Event{
		ev(1, EvCreated, Created{Job: "j", Model: "m", Budget: Budget{Tokens: 10, Steps: 2}}),
		ev(2, EvState, StateChange{State: Pending}),
		ev(3, EvMessage, Message{Role: RoleSystem, Text: "goal"}),
		ev(4, EvMessage, Message{Role: RoleUser, Text: "hi"}),
		ev(5, EvState, StateChange{State: Running}),
		ev(6, EvUsage, Usage{InputTokens: 3, OutputTokens: 2}),
		ev(7, EvMessage, Message{Role: RoleAssistant, Calls: []ToolCall{{ID: "c", Name: "t"}}}),
		ev(8, EvMessage, Message{Role: RoleTool, Results: []ToolResult{{CallID: "c", Untrusted: true}}}),
		ev(9, EvState, StateChange{State: Failed, Reason: "budget"}),
	}
	s, err := Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != Failed || s.Reason != "budget" || s.Steps != 1 || s.Tokens != 5 || !s.Tainted || len(s.History) != 4 {
		t.Errorf("snapshot = %+v", s)
	}
	if len(s.Trusted) != 2 || s.Trusted[0] != "goal" || s.Trusted[1] != "hi" {
		t.Errorf("trusted = %v", s.Trusted)
	}
	if s.Created.Job != "j" {
		t.Errorf("created = %+v", s.Created)
	}
}

func TestReplayRejectsInvalidTransition(t *testing.T) {
	_, err := Replay([]Event{
		ev(1, EvState, StateChange{State: Pending}),
		ev(2, EvState, StateChange{State: Done}),
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("err = %v", err)
	}
}

func TestReplaySumsTokensAndKeepsLastCacheReads(t *testing.T) {
	t.Parallel()
	s, err := Replay([]Event{
		ev(1, EvCreated, Created{Job: "j", Model: "m", Budget: Budget{Tokens: 100, Steps: 5}}),
		ev(2, EvUsage, Usage{InputTokens: 30, OutputTokens: 5, CacheReadTokens: 20}),
		ev(3, EvUsage, Usage{InputTokens: 50, OutputTokens: 10, CacheReadTokens: 40}),
		ev(4, EvUsage, Usage{InputTokens: 20, OutputTokens: 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tokens != 116 || s.Cached != 0 || s.Context != 21 {
		t.Errorf("tokens = %d cached = %d, want the sum of all steps (116) and the last step's cache reads (0)", s.Tokens, s.Cached)
	}
}
