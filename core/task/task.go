package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type State string

const (
	Pending         State = "pending"
	Running         State = "running"
	WaitingApproval State = "waiting_approval"
	Watching        State = "watching"
	Done            State = "done"
	Failed          State = "failed"
)

var ErrInvalidTransition = errors.New("invalid task transition")

var transitions = map[State][]State{
	Pending:         {Running},
	Running:         {Done, Failed, WaitingApproval, Watching},
	WaitingApproval: {Running},
	Watching:        {Running},
}

func Transition(from, to State) error {
	for _, next := range transitions[from] {
		if next == to {
			return nil
		}
	}
	return fmt.Errorf("transition %s -> %s: %w", from, to, ErrInvalidTransition)
}

func (s State) Terminal() bool { return s == Done || s == Failed }

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Write       bool            `json:"write"`
	TargetArg   string          `json:"target_arg,omitempty"`
	Untrusted   bool            `json:"untrusted,omitempty"`
}

type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type ToolResult struct {
	CallID    string `json:"call_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
	Untrusted bool   `json:"untrusted,omitempty"`
	Normal    bool   `json:"normal,omitempty"`
}

type Message struct {
	Role    Role         `json:"role"`
	Text    string       `json:"text,omitempty"`
	Calls   []ToolCall   `json:"calls,omitempty"`
	Results []ToolResult `json:"results,omitempty"`
	// Context marks replayed conversation memory: not a trusted instruction and not a step of this task.
	Context bool `json:"context,omitempty"`
}

// Usage is one model request; InputTokens is the whole prompt, cached parts included.
type Usage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	CacheReadTokens int `json:"cache_read_tokens,omitempty"`
}

type AssistantTurn struct {
	Text  string     `json:"text"`
	Calls []ToolCall `json:"calls,omitempty"`
	Usage Usage      `json:"usage"`
}

type Budget struct {
	Tokens int `json:"tokens"`
	Steps  int `json:"steps"`
}

type EventType string

const (
	EvCreated           EventType = "task.created"
	EvState             EventType = "task.state"
	EvMessage           EventType = "message"
	EvUsage             EventType = "usage"
	EvApprovalRequested EventType = "approval.requested"
	EvApprovalDecided   EventType = "approval.decided"
	EvNoticeSent        EventType = "notice.sent"
)

type Created struct {
	Job       string   `json:"job"`
	Model     string   `json:"model"`
	Tools     []string `json:"tools"`
	Budget    Budget   `json:"budget"`
	Collect   string   `json:"collect,omitempty"`
	NormalMsg string   `json:"normal_msg,omitempty"`
	ReplyTo   string   `json:"reply_to,omitempty"`
	Targets   []string `json:"targets,omitempty"`
	// BrainSource is the provenance the kernel stamps on brain writes from this task; the model cannot set it.
	BrainSource string `json:"brain_source,omitempty"`
	// Silent suppresses the final and failure notices of this task.
	Silent bool `json:"silent,omitempty"`
	// Workdir is the folder an interactive chat was opened in; read tools may only look inside it.
	Workdir string `json:"workdir,omitempty"`
}

type StateChange struct {
	State  State  `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type Event struct {
	Cursor  int64           `json:"cursor"`
	TaskID  string          `json:"task_id"`
	Seq     int             `json:"seq"`
	Type    EventType       `json:"type"`
	Payload json.RawMessage `json:"payload"`
	TS      time.Time       `json:"ts"`
}

type Snapshot struct {
	Created Created
	State   State
	Reason  string
	History []Message
	Trusted []string
	Tainted bool
	Steps   int
	Tokens  int
	// Context is the last step's prompt plus output; Cached is that step's cache reads.
	Context int
	Cached  int
}

type LedgerState string

const (
	LedgerPlanned        LedgerState = "planned"
	LedgerApproved       LedgerState = "approved"
	LedgerDispatching    LedgerState = "dispatching"
	LedgerSucceeded      LedgerState = "succeeded"
	LedgerFailed         LedgerState = "failed"
	LedgerOutcomeUnknown LedgerState = "outcome_unknown"
)

type LedgerEntry struct {
	IdemKey    string      `json:"idem_key"`
	TaskID     string      `json:"task_id"`
	Tool       string      `json:"tool"`
	Target     string      `json:"target"`
	ArgsHash   string      `json:"args_hash"`
	ApprovalID string      `json:"approval_id,omitempty"`
	State      LedgerState `json:"state"`
	Result     string      `json:"result,omitempty"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// Replay rebuilds a task snapshot from its events in seq order.
func Replay(events []Event) (Snapshot, error) {
	var s Snapshot
	for _, e := range events {
		switch e.Type {
		case EvCreated:
			if err := json.Unmarshal(e.Payload, &s.Created); err != nil {
				return Snapshot{}, fmt.Errorf("decode created event seq %d: %w", e.Seq, err)
			}
		case EvState:
			var c StateChange
			if err := json.Unmarshal(e.Payload, &c); err != nil {
				return Snapshot{}, fmt.Errorf("decode state event seq %d: %w", e.Seq, err)
			}
			if s.State == "" && c.State == Pending {
				s.State = Pending
				continue
			}
			if err := Transition(s.State, c.State); err != nil {
				return Snapshot{}, fmt.Errorf("replay seq %d: %w", e.Seq, err)
			}
			s.State, s.Reason = c.State, c.Reason
		case EvMessage:
			var m Message
			if err := json.Unmarshal(e.Payload, &m); err != nil {
				return Snapshot{}, fmt.Errorf("decode message event seq %d: %w", e.Seq, err)
			}
			s.History = append(s.History, m)
			if m.Context {
				continue
			}
			switch m.Role {
			case RoleSystem, RoleUser:
				s.Trusted = append(s.Trusted, m.Text)
			case RoleAssistant:
				s.Steps++
			case RoleTool:
				for _, r := range m.Results {
					s.Tainted = s.Tainted || r.Untrusted
				}
			}
		case EvUsage:
			var u Usage
			if err := json.Unmarshal(e.Payload, &u); err != nil {
				return Snapshot{}, fmt.Errorf("decode usage event seq %d: %w", e.Seq, err)
			}
			s.Tokens += u.InputTokens + u.OutputTokens
			s.Context, s.Cached = u.InputTokens+u.OutputTokens, u.CacheReadTokens
		}
	}
	return s, nil
}
