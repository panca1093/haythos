package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

var ErrNotImplemented = errors.New("not implemented")

type Tools interface {
	List(scope []string) []task.ToolSpec
	Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error)
}

type Ledger interface {
	Get(ctx context.Context, idemKey string) (task.LedgerEntry, bool, error)
	Put(ctx context.Context, e task.LedgerEntry) error
}

type Vault interface {
	Resolve(handle string) (string, error)
}

type Deps struct {
	Tools       Tools
	Ledger      Ledger
	Vault       Vault
	Policy      policy.Policy
	Clock       func() time.Time
	ApprovalTTL time.Duration
	Paused      func() bool
	Hook        func(point string) error
}

type Request struct {
	TaskID    string
	Call      task.ToolCall
	Allowlist []string
	Tainted   bool
	Trusted   []string
	Approval  *policy.Approval
}

type Pending struct {
	Approval  policy.Approval
	Tier      policy.Tier
	Escalated bool
	Preview   string
}

type Outcome struct {
	Result  task.ToolResult
	Pending *Pending
}

type Gateway struct{ d Deps }

func New(d Deps) *Gateway { return &Gateway{d: d} }

// Execute is the only path to run a tool: allowlist, tier, taint, approval, ledger, secrets, execute.
func (g *Gateway) Execute(ctx context.Context, req Request) (Outcome, error) {
	return Outcome{}, ErrNotImplemented
}
