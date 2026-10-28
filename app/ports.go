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

type NoticeKind string

const (
	NoticeMessage  NoticeKind = "message.done"
	NoticeApproval NoticeKind = "approval.request"
	NoticeState    NoticeKind = "task.state"
	NoticeAlert    NoticeKind = "alert"
)

type ApprovalRequest struct {
	Approval  policy.Approval
	Tier      policy.Tier
	Escalated bool
	Preview   string
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
}

type Input struct {
	Text string
	// ReplyTo is the channel adapter's address for this input's notices; empty means its default.
	ReplyTo string
}

type ModelChoice struct {
	Value       string
	Name        string
	Description string
}

type ModelSelector interface {
	List(ctx context.Context) ([]ModelChoice, error)
	Current() string
	Select(ctx context.Context, value string) error
}
