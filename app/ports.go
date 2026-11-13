package app

import (
	"context"
	"errors"

	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

var ErrCannotVerify = errors.New("cannot verify destination")

type Model interface {
	Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error)
}

type Tools interface {
	List(scope []string) []task.ToolSpec
	Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error)
	// Exists asks the destination whether the write identified by idemKey already happened.
	Exists(ctx context.Context, tool, target, idemKey string) (bool, error)
}

type EventStore interface {
	Append(ctx context.Context, taskID string, typ task.EventType, payload any) (task.Event, error)
	Load(ctx context.Context, taskID string) ([]task.Event, error)
	Since(ctx context.Context, cursor int64) ([]task.Event, error)
	TasksIn(ctx context.Context, state task.State) ([]string, error)
}

type Ledger interface {
	Get(ctx context.Context, idemKey string) (task.LedgerEntry, bool, error)
	Put(ctx context.Context, e task.LedgerEntry) error
	InState(ctx context.Context, s task.LedgerState) ([]task.LedgerEntry, error)
}

type Vault interface {
	Resolve(handle string) (string, error)
}

type BrainItem struct {
	Slug       string
	Title      string
	Text       string
	Source     string
	Unverified bool
}

type Brain interface {
	Recall(ctx context.Context, query string, limit int) ([]BrainItem, error)
	Remember(ctx context.Context, item BrainItem) (slug string, err error)
	Forget(ctx context.Context, slug string) error
}

type NoticeKind string

const (
	NoticeMessage  NoticeKind = "message.done"
	NoticeApproval NoticeKind = "approval.request"
	NoticeState    NoticeKind = "task.state"
	NoticeAlert    NoticeKind = "alert"
	NoticeDigest   NoticeKind = "brain.digest"
	// NoticeTool is best-effort progress for interactive clients; channels may drop it.
	NoticeTool NoticeKind = "tool.started"
)

type ApprovalRequest struct {
	Approval  policy.Approval
	Tier      policy.Tier
	Escalated bool
	Preview   string
}

type UndoAction struct {
	ID    string
	Label string
}

type Notice struct {
	Key      string
	Kind     NoticeKind
	TaskID   string
	Text     string
	State    task.State
	Reason   string
	Approval *ApprovalRequest
	ReplyTo  string
	// Tainted marks a notice from a task that read untrusted content; channels must not unfurl links in it.
	Tainted bool
	// Context is the last step's size against the model's Window, and Cached its cache reads; set on the final message.
	Context, Window, Cached int
	// Undo are the per-item Undo buttons of a digest; the channel passes the chosen ID to App.UndoBrain.
	Undo []UndoAction
}

type Notifier interface {
	Publish(ctx context.Context, n Notice) error
}

type Trigger struct {
	Cron   string `yaml:"cron"`
	Manual bool   `yaml:"manual"`
}

type Job struct {
	Name      string
	Goal      string
	Model     string
	Tools     []string
	Budget    task.Budget
	Trigger   Trigger
	Collect   string
	NormalMsg string
	// DeliverTo are channel-adapter addresses for this job's notices; empty means the adapter's default.
	DeliverTo []string
	// Silent suppresses the task's final and failure notices.
	Silent bool
	// BrainSource is the provenance stamped on brain writes from this job's tasks.
	BrainSource string
	// Runtime is the alias of the runtime this job delegates to; empty runs on the kernel's loop.
	Runtime string
	// Workdir, RuntimeModel, RuntimeMode and Checks apply only when Runtime is set; Workdir is already resolved from config.
	Workdir      string
	RuntimeModel string
	RuntimeMode  string
	Checks       []Check
}

type Input struct {
	Text  string
	Model string
	// ReplyTo is the channel adapter's address for this input's notices; empty means its default.
	ReplyTo string
	// DM marks ReplyTo as a direct-message channel; conversation memory only looks back 24 h there.
	DM bool
	// Workdir is the folder the chat was opened in; empty when the channel has none.
	Workdir string
}

type ModelChoice struct {
	Value       string
	Name        string
	Description string
}

type ModelVariants interface {
	List(ctx context.Context) ([]ModelChoice, error)
	WithModel(name string) Model
}

type ModelSelector interface {
	List(ctx context.Context) ([]ModelChoice, error)
	Current() string
	Select(ctx context.Context, value string) error
}

type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
)

type RuntimeCaps struct {
	Resume bool
	Images bool
	Modes  []string
}

type RuntimeStart struct {
	TaskID, SessionID string
	Workdir           string
	Model, Mode       string
	// ResumeRef is the runtime's own session id; empty starts a new session.
	ResumeRef string
}

type RuntimeEventKind string

const (
	RuntimeMessage           RuntimeEventKind = "message"
	RuntimeThought           RuntimeEventKind = "thought"
	RuntimeToolCall          RuntimeEventKind = "tool_call"
	RuntimeToolCallUpdate    RuntimeEventKind = "tool_call_update"
	RuntimePlan              RuntimeEventKind = "plan"
	RuntimePermissionRequest RuntimeEventKind = "permission_request"
	RuntimeMode              RuntimeEventKind = "mode"
	RuntimeStop              RuntimeEventKind = "stop"
)

// PermissionRequest is a runtime asking to run one guarded step; Text is what the guard policy matches on.
type PermissionRequest struct {
	RequestID string `json:"request_id"`
	Tool      string `json:"tool"`
	// Kind is the ACP tool kind: execute, read, edit, delete, ...
	Kind string `json:"kind"`
	// Text is the shell command or the path the request acts on.
	Text           string `json:"text"`
	AllowOptionID  string `json:"allow_option_id"`
	RejectOptionID string `json:"reject_option_id"`
}

// RuntimeEvent is one ACP-shaped event of a session. message and thought events are chunks.
type RuntimeEvent struct {
	Kind RuntimeEventKind `json:"kind"`
	// ID is the message id or tool call id the event belongs to; empty when the runtime has none.
	ID         string             `json:"id,omitempty"`
	Text       string             `json:"text,omitempty"`
	ToolKind   string             `json:"tool_kind,omitempty"`
	Status     string             `json:"status,omitempty"`
	Permission *PermissionRequest `json:"permission,omitempty"`
	Stop       StopReason         `json:"stop,omitempty"`
}

type Runtime interface {
	Name() string
	Capabilities() RuntimeCaps
	Start(ctx context.Context, req RuntimeStart) (RuntimeSession, error)
}

// RuntimeSession: Send returns only after the turn's events, the closing stop included, were handed to Events.
type RuntimeSession interface {
	Ref() string
	Send(ctx context.Context, prompt string) (StopReason, error)
	Approve(ctx context.Context, requestID, optionID string) error
	Events() <-chan RuntimeEvent
	Stop(ctx context.Context) error
}
