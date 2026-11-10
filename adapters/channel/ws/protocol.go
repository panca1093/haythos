package ws

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/panca/haythos/app"
)

// Prefix marks the conversations WS clients own; they may act on no other origin.
const Prefix = "tui:"

const (
	TypeHello        = "hello"
	TypeSubscribe    = "subscribe"
	TypeInput        = "input"
	TypeDecide       = "approval.decide"
	TypeSessions     = "sessions"
	TypeWelcome      = "welcome"
	TypeHistory      = "history"
	TypeNotice       = "notice"
	TypeSessionsList = "sessions.list"
	TypeError        = "error"
)

type Hello struct {
	Client   string `json:"client"`
	Instance string `json:"instance"`
	Version  string `json:"version"`
}

type Subscribe struct {
	Conv string `json:"conv"`
	// Workdir is the absolute folder the chat was opened in; read tools are confined to it.
	Workdir string `json:"workdir,omitempty"`
}

type Input struct {
	Conv string `json:"conv"`
	Text string `json:"text"`
}

type Decide struct {
	TaskID     string `json:"task_id"`
	ApprovalID string `json:"approval_id"`
	Approve    bool   `json:"approve"`
}

type Sessions struct{}

func (f Hello) MarshalJSON() ([]byte, error) {
	type plain Hello
	return typed(TypeHello, plain(f))
}

func (f Subscribe) MarshalJSON() ([]byte, error) {
	type plain Subscribe
	return typed(TypeSubscribe, plain(f))
}

func (f Input) MarshalJSON() ([]byte, error) {
	type plain Input
	return typed(TypeInput, plain(f))
}

func (f Decide) MarshalJSON() ([]byte, error) {
	type plain Decide
	return typed(TypeDecide, plain(f))
}

func (Sessions) MarshalJSON() ([]byte, error) { return typed(TypeSessions, struct{}{}) }

// typed splices the frame type into the JSON object of body.
func typed(typ string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode %s frame: %w", typ, err)
	}
	head, err := json.Marshal(map[string]string{"type": typ})
	if err != nil {
		return nil, fmt.Errorf("encode %s frame: %w", typ, err)
	}
	if string(b) == "{}" {
		return head, nil
	}
	return append(append(head[:len(head)-1], ','), b[1:]...), nil
}

// clientFrame is any client frame as the server decodes it.
type clientFrame struct {
	Type       string `json:"type"`
	Client     string `json:"client"`
	Instance   string `json:"instance"`
	Version    string `json:"version"`
	Conv       string `json:"conv"`
	Workdir    string `json:"workdir"`
	Text       string `json:"text"`
	TaskID     string `json:"task_id"`
	ApprovalID string `json:"approval_id"`
	Approve    bool   `json:"approve"`
}

// Approval is what a client may see of an approval request: never tool arguments beyond the preview.
type Approval struct {
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	Target    string `json:"target"`
	Tier      int    `json:"tier"`
	Preview   string `json:"preview"`
	Escalated bool   `json:"escalated"`
}

type Turn struct {
	User   string    `json:"user"`
	Answer string    `json:"answer"`
	At     time.Time `json:"at"`
}

type Pending struct {
	TaskID   string   `json:"task_id"`
	Approval Approval `json:"approval"`
}

type Session struct {
	Conv      string    `json:"conv"`
	FirstUser string    `json:"first_user"`
	LastAt    time.Time `json:"last_at"`
}

// Frame is any server frame; Type says which fields are set.
type Frame struct {
	Type string `json:"type"`

	Version   string    `json:"version,omitempty"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Channels  []string  `json:"channels,omitempty"`
	// Model is the model value in use ("" = the provider default); DefaultModel is its config name.
	Model        string `json:"model,omitempty"`
	DefaultModel string `json:"default_model,omitempty"`

	Conv    string    `json:"conv,omitempty"`
	Turns   []Turn    `json:"turns,omitempty"`
	Pending []Pending `json:"pending,omitempty"`

	UserMessages int `json:"user_messages,omitempty"`
	ToolCalls    int `json:"tool_calls,omitempty"`

	Kind     string    `json:"kind,omitempty"`
	TaskID   string    `json:"task_id,omitempty"`
	Text     string    `json:"text,omitempty"`
	State    string    `json:"state,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
	Tokens   int       `json:"tokens,omitempty"`
	Budget   int       `json:"budget,omitempty"`

	Items []Session `json:"items,omitempty"`

	Message string `json:"message,omitempty"`
}

func approvalOf(r app.ApprovalRequest) Approval {
	return Approval{
		ID: r.Approval.ID, Tool: r.Approval.Tool, Target: r.Approval.Target,
		Tier: int(r.Tier), Preview: r.Preview, Escalated: r.Escalated,
	}
}

func historyFrame(conv string, v app.ConversationView) Frame {
	f := Frame{Type: TypeHistory, Conv: conv, UserMessages: v.UserMessages, ToolCalls: v.ToolCalls}
	for _, t := range v.Turns {
		f.Turns = append(f.Turns, Turn{User: t.User, Answer: t.Answer, At: t.At})
	}
	for _, p := range v.Pending {
		f.Pending = append(f.Pending, Pending{TaskID: p.TaskID, Approval: approvalOf(p.Approval)})
	}
	return f
}

func noticeFrame(n app.Notice) Frame {
	f := Frame{Type: TypeNotice, Kind: string(n.Kind), TaskID: n.TaskID, Text: n.Text, State: string(n.State), Reason: n.Reason, Tokens: n.Tokens, Budget: n.Budget}
	if n.Approval != nil {
		a := approvalOf(*n.Approval)
		f.Approval = &a
	}
	return f
}
