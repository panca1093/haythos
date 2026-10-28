package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

type Tier int

const (
	TierRead Tier = iota
	TierInternalWrite
	TierExternalWrite
	TierHighImpact
	TierForbidden
)

type Action string

const (
	Allow Action = "allow"
	Ask   Action = "ask"
	Deny  Action = "deny"
)

type Policy struct {
	Version string
	Tiers   map[string]Tier
	// Secrets lists, per tool, the vault names its arguments may reference; any other handle is exfiltration.
	Secrets map[string][]string
	// AutoWhenClean lists tier-1 writes that run without approval in an untainted owner or background-brain task.
	AutoWhenClean []string
}

type Input struct {
	Tool          string
	Write         bool
	Tainted       bool
	TargetTrusted bool
	// Source is the task's brain provenance ("owner/...", "auto/..."); empty for tasks that cannot write to memory.
	Source string
	// Target is the write target the tool call names, if any.
	Target  string
	Secrets []string
}

type Decision struct {
	Action    Action
	Tier      Tier
	Escalated bool
	Reason    string
}

type Approval struct {
	ID            string    `json:"id"`
	Tool          string    `json:"tool"`
	Target        string    `json:"target"`
	ArgsHash      string    `json:"args_hash"`
	Expiry        time.Time `json:"expiry"`
	PolicyVersion string    `json:"policy_version"`
}

// autoSource: owner tasks always qualify; background (auto/) tasks may forget only inbox items.
func autoSource(in Input) bool {
	switch {
	case strings.HasPrefix(in.Source, "owner/"):
		return true
	case strings.HasPrefix(in.Source, "auto/"):
		return in.Tool != "brain.forget" || strings.HasPrefix(in.Target, "inbox/")
	}
	return false
}

var ErrApprovalVoid = errors.New("approval void")

// Evaluate applies M1–M2 rules: reads auto, every write asks, tier 4 and unbound secrets denied.
func (p Policy) Evaluate(in Input) Decision {
	tier, ok := p.Tiers[in.Tool]
	if !ok {
		return Decision{Action: Deny, Tier: TierForbidden, Reason: "no tier configured"}
	}
	for _, h := range in.Secrets {
		if !slices.Contains(p.Secrets[in.Tool], strings.TrimPrefix(h, "secret://")) {
			return Decision{Action: Deny, Tier: TierForbidden, Reason: h + " is not bound to this tool"}
		}
	}
	d := Decision{Tier: tier}
	if in.Write && d.Tier < TierInternalWrite {
		d.Tier = TierInternalWrite
	}
	if in.Write && in.Tainted && !in.TargetTrusted && d.Tier < TierHighImpact {
		d.Tier, d.Escalated = TierHighImpact, true
		d.Reason = "target did not come from a trusted instruction"
	}
	switch {
	case d.Tier == TierRead:
		d.Action = Allow
	case d.Tier == TierInternalWrite && !in.Tainted && slices.Contains(p.AutoWhenClean, in.Tool) && autoSource(in):
		d.Action = Allow
	case d.Tier == TierForbidden:
		d.Action = Deny
		d.Reason = "forbidden tier"
	default:
		d.Action = Ask
	}
	return d
}

// ArgsHash hashes args canonically (key order independent).
func ArgsHash(args json.RawMessage) string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		v = string(args)
	}
	canon, _ := json.Marshal(v)
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}

// ValidateApproval returns an error wrapping ErrApprovalVoid on any mismatch.
func ValidateApproval(a Approval, tool, target, argsHash string, now time.Time, policyVersion string) error {
	switch {
	case a.Tool != tool:
		return fmt.Errorf("%w: tool changed", ErrApprovalVoid)
	case a.Target != target:
		return fmt.Errorf("%w: target changed", ErrApprovalVoid)
	case a.ArgsHash != argsHash:
		return fmt.Errorf("%w: arguments changed", ErrApprovalVoid)
	case a.PolicyVersion != policyVersion:
		return fmt.Errorf("%w: policy version changed", ErrApprovalVoid)
	case !now.Before(a.Expiry):
		return fmt.Errorf("%w: expired", ErrApprovalVoid)
	}
	return nil
}
