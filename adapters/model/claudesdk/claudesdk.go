package claudesdk

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/panca/haythos/core/task"
)

var (
	ErrNotImplemented = errors.New("not implemented")
	ErrInvalidOutput  = errors.New("invalid model output")
)

// StepRequest is the sidecar "step" call. Tools lists SDK built-in tools to enable and must always be empty.
type StepRequest struct {
	System string          `json:"system"`
	Prompt string          `json:"prompt"`
	Schema json.RawMessage `json:"schema"`
	Tools  []string        `json:"tools"`
}

type StepResponse struct {
	Text  string     `json:"text"`
	Usage task.Usage `json:"usage"`
}

type Transport interface {
	Step(ctx context.Context, req StepRequest) (StepResponse, error)
}

type Adapter struct{ t Transport }

func New(t Transport) *Adapter { return &Adapter{t: t} }

func (a *Adapter) Step(ctx context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	return task.AssistantTurn{}, ErrNotImplemented
}
