package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type Tools interface {
	List(scope []string) []task.ToolSpec
	Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error)
}

type Ledger interface {
	Get(ctx context.Context, idemKey string) (task.LedgerEntry, bool, error)
	Put(ctx context.Context, e task.LedgerEntry) error
	InState(ctx context.Context, s task.LedgerState) ([]task.LedgerEntry, error)
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
	// BrainSource is the task's brain provenance, used for auto-approval.
	BrainSource string
	Trusted     []string
	Approval    *policy.Approval
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
	// Unknown means the write may or may not have happened; the caller must tell the user.
	Unknown bool
}

type Gateway struct{ d Deps }

func New(d Deps) *Gateway {
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.ApprovalTTL == 0 {
		d.ApprovalTTL = time.Hour
	}
	if d.Paused == nil {
		d.Paused = func() bool { return false }
	}
	if d.Hook == nil {
		d.Hook = func(string) error { return nil }
	}
	return &Gateway{d: d}
}

var secretRef = regexp.MustCompile(`secret://[A-Za-z0-9_.-]+`)

// Execute is the only path to run a tool: allowlist, tier, taint, approval, ledger, secrets, execute.
func (g *Gateway) Execute(ctx context.Context, req Request) (Outcome, error) {
	call := req.Call
	spec, ok := findSpec(g.d.Tools.List(req.Allowlist), call.Name)
	if !ok {
		return denied(call, fmt.Sprintf("tool %q is not allowed for this task", call.Name)), nil
	}
	target := targetOf(spec, call.Args)
	dec := g.d.Policy.Evaluate(policy.Input{
		Tool: spec.Name, Write: spec.Write, Tainted: req.Tainted,
		TargetTrusted: target != "" && mentioned(req.Trusted, target),
		Source:        req.BrainSource, Target: target,
		Secrets: secretRef.FindAllString(string(call.Args), -1),
	})
	if dec.Action == policy.Deny {
		return denied(call, "denied by policy: "+dec.Reason), nil
	}
	if spec.Write && g.d.Paused() {
		return denied(call, "paused: writes are stopped"), nil
	}

	idem := req.TaskID + "/" + call.ID
	hash := policy.ArgsHash(call.Args)
	var entry task.LedgerEntry
	if spec.Write {
		prior, found, err := g.d.Ledger.Get(ctx, idem)
		if err != nil {
			return Outcome{}, fmt.Errorf("read ledger: %w", err)
		}
		if found && prior.State != task.LedgerPlanned && prior.State != task.LedgerApproved {
			return Outcome{Result: task.ToolResult{
				CallID: call.ID, Content: fmt.Sprintf("not re-executed: action is %s. %s", prior.State, prior.Result),
				IsError: prior.State != task.LedgerSucceeded, Untrusted: spec.Untrusted,
			}}, nil
		}
		entry = prior
		if u, found, err := g.unknownTwin(ctx, req.TaskID, spec.Name, target); err != nil {
			return Outcome{}, err
		} else if found {
			return denied(call, fmt.Sprintf("not executed: an earlier %s to %q in this task has an unknown outcome (%s). The user must check it first.", spec.Name, target, u.IdemKey)), nil
		}
	}

	if dec.Action == policy.Ask {
		if req.Approval == nil {
			return Outcome{Pending: &Pending{
				Approval: policy.Approval{
					ID: "ap-" + call.ID, Tool: spec.Name, Target: target, ArgsHash: hash,
					Expiry: g.d.Clock().Add(g.d.ApprovalTTL), PolicyVersion: g.d.Policy.Version,
				},
				Tier: dec.Tier, Escalated: dec.Escalated, Preview: preview(spec.Name, call.Args),
			}}, nil
		}
		if err := policy.ValidateApproval(*req.Approval, spec.Name, target, hash, g.d.Clock(), g.d.Policy.Version); err != nil {
			return Outcome{}, fmt.Errorf("validate approval for %s: %w", spec.Name, err)
		}
	}

	if !spec.Write {
		res, err := g.invoke(ctx, call, spec)
		return Outcome{Result: res}, err
	}

	entry.IdemKey, entry.TaskID, entry.Tool, entry.Target, entry.ArgsHash = idem, req.TaskID, spec.Name, target, hash
	if req.Approval != nil {
		entry.ApprovalID = req.Approval.ID
	}
	for _, st := range []task.LedgerState{task.LedgerPlanned, task.LedgerApproved} {
		if err := g.setLedger(ctx, &entry, st, ""); err != nil {
			return Outcome{}, err
		}
	}
	if err := g.d.Hook("gateway.before_dispatch"); err != nil {
		return Outcome{}, err
	}
	if g.d.Paused() {
		return denied(call, "paused: writes are stopped"), nil
	}
	if err := g.setLedger(ctx, &entry, task.LedgerDispatching, ""); err != nil {
		return Outcome{}, err
	}

	res, execErr := g.invoke(ctx, call, spec)
	if err := g.d.Hook("gateway.after_execute"); err != nil {
		return Outcome{}, err
	}
	final, content := task.LedgerSucceeded, res.Content
	switch {
	case execErr != nil && (errors.Is(execErr, context.Canceled) || errors.Is(execErr, context.DeadlineExceeded)):
		final, content = task.LedgerOutcomeUnknown, "interrupted: "+res.Content
	case execErr != nil:
		// A transport error after dispatch cannot tell sent from not sent; only a tool-reported error is a definite failure.
		final, content = task.LedgerOutcomeUnknown, "outcome unknown: "+res.Content
	case res.IsError:
		final = task.LedgerFailed
	}
	if err := g.setLedger(ctx, &entry, final, content); err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: res, Unknown: final == task.LedgerOutcomeUnknown}, nil
}

func (g *Gateway) unknownTwin(ctx context.Context, taskID, tool, target string) (task.LedgerEntry, bool, error) {
	rows, err := g.d.Ledger.InState(ctx, task.LedgerOutcomeUnknown)
	if err != nil {
		return task.LedgerEntry{}, false, fmt.Errorf("read ledger: %w", err)
	}
	for _, r := range rows {
		if r.TaskID == taskID && r.Tool == tool && r.Target == target {
			return r, true, nil
		}
	}
	return task.LedgerEntry{}, false, nil
}

func (g *Gateway) invoke(ctx context.Context, call task.ToolCall, spec task.ToolSpec) (task.ToolResult, error) {
	args, values, err := g.resolveSecrets(call.Args)
	if err != nil {
		return task.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true}, nil //nolint:nilerr // unresolvable secret is a model-visible tool error, not a kernel failure
	}
	real := call
	real.Args = args
	res, err := g.d.Tools.Call(ctx, real)
	res.CallID = call.ID
	res.Untrusted = spec.Untrusted
	if err != nil {
		res.Content, res.IsError = scrub(err.Error(), values), true
		return res, err
	}
	res.Content = scrub(res.Content, values)
	if spec.Untrusted {
		res.Content = task.StripInvisible(res.Content)
	}
	return res, nil
}

func (g *Gateway) setLedger(ctx context.Context, e *task.LedgerEntry, st task.LedgerState, result string) error {
	e.State, e.UpdatedAt = st, g.d.Clock()
	if result != "" {
		e.Result = result
	}
	if err := g.d.Ledger.Put(ctx, *e); err != nil {
		return fmt.Errorf("write ledger %s: %w", st, err)
	}
	return nil
}

// resolveSecrets swaps secret://handle for its value; handles are the only form the model and the log ever see.
func (g *Gateway) resolveSecrets(args json.RawMessage) (json.RawMessage, map[string]string, error) {
	values := map[string]string{}
	var firstErr error
	out := secretRef.ReplaceAllStringFunc(string(args), func(handle string) string {
		v, err := g.d.Vault.Resolve(handle)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("resolve %s: %w", handle, err)
			}
			return handle
		}
		values[handle] = v
		quoted, _ := json.Marshal(v)
		return string(quoted[1 : len(quoted)-1])
	})
	if firstErr != nil {
		return nil, nil, firstErr
	}
	return json.RawMessage(out), values, nil
}

func scrub(s string, values map[string]string) string {
	for handle, v := range values {
		if v != "" {
			s = strings.ReplaceAll(s, v, handle)
		}
	}
	return s
}

func denied(call task.ToolCall, msg string) Outcome {
	return Outcome{Result: task.ToolResult{CallID: call.ID, Content: msg, IsError: true}}
}

func findSpec(specs []task.ToolSpec, name string) (task.ToolSpec, bool) {
	for _, s := range specs {
		if s.Name == name {
			return s, true
		}
	}
	return task.ToolSpec{}, false
}

func targetOf(spec task.ToolSpec, args json.RawMessage) string {
	if spec.TargetArg == "" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(args, &m) != nil {
		return ""
	}
	s, _ := m[spec.TargetArg].(string)
	return s
}

// mentioned reports whether target appears as a whole whitespace-separated token, ignoring surrounding punctuation.
// TODO: uncertain — a negated mention ("don't post to #x") still counts as trusted, verify before prod
func mentioned(trusted []string, target string) bool {
	for _, t := range trusted {
		for _, w := range strings.Fields(t) {
			if strings.Trim(w, ".,;:!?()[]{}<>\"'`") == target {
				return true
			}
		}
	}
	return false
}

func preview(tool string, args json.RawMessage) string {
	s := string(args)
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	return tool + " " + s
}
