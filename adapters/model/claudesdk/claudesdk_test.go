package claudesdk

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/panca/haythos/core/task"
)

func TestValidate(t *testing.T) {
	schema := map[string]any{}
	_ = json.Unmarshal([]byte(`{"type":"object","required":["n","tags"],"properties":{"n":{"type":"integer"},"tags":{"type":"array","items":{"type":"string"}},"mode":{"enum":["a","b"]}}}`), &schema)
	for _, tc := range []struct {
		name    string
		args    string
		wantErr bool
	}{
		{"ok", `{"n":2,"tags":["x"],"mode":"a"}`, false},
		{"missing required", `{"n":2}`, true},
		{"wrong type", `{"n":"2","tags":[]}`, true},
		{"non integer", `{"n":2.5,"tags":[]}`, true},
		{"bad item", `{"n":1,"tags":[1]}`, true},
		{"bad enum", `{"n":1,"tags":[],"mode":"c"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v any
			_ = json.Unmarshal([]byte(tc.args), &v)
			if err := validate(schema, v, "args"); (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestParseOutputAcceptsFencedJSONAndAssignsStableIDs(t *testing.T) {
	tools := []task.ToolSpec{{Name: "vm.read", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	raw := "```json\n{\"text\":\"\",\"tool_calls\":[{\"name\":\"vm.read\",\"args\":{}}]}\n```"
	a, err := parseOutput(raw, tools, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := parseOutput(raw, tools, 4)
	if a.Calls[0].ID != "sdk-4-0" || a.Calls[0].ID != b.Calls[0].ID {
		t.Errorf("ids = %q %q", a.Calls[0].ID, b.Calls[0].ID)
	}
}

type captureTransport struct{ reqs []StepRequest }

func (c *captureTransport) Step(_ context.Context, req StepRequest) (StepResponse, error) {
	c.reqs = append(c.reqs, req)
	return StepResponse{Text: `{"text":"ok"}`}, nil
}

func TestStepRequestCarriesConfiguredModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
		want  string
		json  bool
	}{
		{"set", "claude-sonnet-4-5", "claude-sonnet-4-5", true},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &captureTransport{}
			if _, err := New(tr, WithModel(tc.model)).Step(context.Background(), []task.Message{{Role: task.RoleUser, Text: "hi"}}, nil); err != nil {
				t.Fatal(err)
			}
			if got := tr.reqs[0].Model; got != tc.want {
				t.Errorf("Model = %q, want %q", got, tc.want)
			}
			raw, _ := json.Marshal(tr.reqs[0])
			if has := strings.Contains(string(raw), `"model"`); has != tc.json {
				t.Errorf("model key present = %v, want %v: %s", has, tc.json, raw)
			}
		})
	}
}

func TestSetModelAppliesToNextStep(t *testing.T) {
	tr := &captureTransport{}
	a := New(tr, WithModel("opus"))
	if a.Model() != "opus" {
		t.Errorf("Model() = %q, want opus", a.Model())
	}
	history := []task.Message{{Role: task.RoleUser, Text: "hi"}}
	for _, model := range []string{"haiku", ""} {
		a.SetModel(model)
		if a.Model() != model {
			t.Errorf("Model() = %q, want %q", a.Model(), model)
		}
		if _, err := a.Step(context.Background(), history, nil); err != nil {
			t.Fatal(err)
		}
		if got := tr.reqs[len(tr.reqs)-1].Model; got != model {
			t.Errorf("request Model = %q, want %q", got, model)
		}
	}
}

type lockedTransport struct {
	mu     sync.Mutex
	models []string
}

func (l *lockedTransport) Step(_ context.Context, req StepRequest) (StepResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.models = append(l.models, req.Model)
	return StepResponse{Text: `{"text":"ok"}`}, nil
}

func TestSetModelIsSafeAgainstConcurrentSteps(t *testing.T) {
	a := New(&lockedTransport{}, WithModel("opus"))
	history := []task.Message{{Role: task.RoleUser, Text: "hi"}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				a.SetModel("haiku")
				_ = a.Model()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := a.Step(context.Background(), history, nil); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}

type usageTransport struct{}

func (usageTransport) Step(_ context.Context, _ StepRequest) (StepResponse, error) {
	return StepResponse{Text: `{"text":"ok"}`, Usage: task.Usage{InputTokens: 950, OutputTokens: 5, CacheReadTokens: 900}}, nil
}

func TestStepKeepsCacheReads(t *testing.T) {
	t.Parallel()
	turn, err := New(usageTransport{}).Step(context.Background(), []task.Message{{Role: task.RoleUser, Text: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Usage.CacheReadTokens != 900 {
		t.Errorf("usage = %+v, want cache reads passed through", turn.Usage)
	}
}
