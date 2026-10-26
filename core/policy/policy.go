package policy

import (
	"encoding/json"
	"errors"
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
}

type Input struct {
	Tool          string
	Write         bool
	Tainted       bool
	TargetTrusted bool
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

var (
	ErrApprovalVoid = errors.New("approval void")
	errNotImpl      = errors.New("not implemented")
)

func (p Policy) Evaluate(in Input) Decision {
	return Decision{Action: Deny, Reason: errNotImpl.Error()}
}

// ArgsHash hashes args canonically (key order independent).
func ArgsHash(args json.RawMessage) string { return "" }

// ValidateApproval returns an error wrapping ErrApprovalVoid on any mismatch.
func ValidateApproval(a Approval, tool, target, argsHash string, now time.Time, policyVersion string) error {
	return errNotImpl
}
