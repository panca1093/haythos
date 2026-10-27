package claudesdk

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// TestHelperProcess is a fake sidecar. It crashes on its first "step" when HELPER_CRASH_MARKER does not exist yet.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("HELPER_SIDECAR") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req rpcRequest
		_ = json.Unmarshal(sc.Bytes(), &req)
		switch req.Method {
		case "ping":
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":\"pong\"}\n", req.ID)
		case "models":
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":[{\"value\":\"opus\",\"displayName\":\"Opus\",\"description\":\"Best\"},{\"value\":\"haiku\",\"displayName\":\"Haiku\",\"description\":\"Fast\"}]}\n", req.ID)
		case "step":
			if m := os.Getenv("HELPER_CRASH_MARKER"); m != "" {
				if _, err := os.Stat(m); err != nil {
					_ = os.WriteFile(m, nil, 0o600)
					os.Exit(3)
				}
			}
			fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"text\":\"{}\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n", req.ID)
		}
	}
	os.Exit(0)
}

func helperConfig(t *testing.T, marker string) SidecarConfig {
	t.Helper()
	return SidecarConfig{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env:     []string{"HELPER_SIDECAR=1", "HELPER_CRASH_MARKER=" + marker},
	}
}

func TestSidecarStepAndHealthCheck(t *testing.T) {
	sc, err := StartSidecar(context.Background(), helperConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close() }()
	resp, err := sc.Step(context.Background(), StepRequest{Tools: []string{}})
	if err != nil || resp.Text != "{}" || resp.Usage.InputTokens != 1 {
		t.Errorf("resp=%+v err=%v", resp, err)
	}
}

func TestSidecarRestartsAfterCrash(t *testing.T) {
	marker := t.TempDir() + "/crashed"
	sc, err := StartSidecar(context.Background(), helperConfig(t, marker))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close() }()
	resp, err := sc.Step(context.Background(), StepRequest{Tools: []string{}})
	if err != nil || resp.Text != "{}" {
		t.Errorf("resp=%+v err=%v", resp, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("helper never crashed; test did not exercise restart")
	}
}

func TestSidecarStartFailsWhenBinaryMissing(t *testing.T) {
	if _, err := StartSidecar(context.Background(), SidecarConfig{Command: "/nonexistent/sidecar"}); err == nil {
		t.Error("want error")
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

func TestSidecarModels(t *testing.T) {
	sc, err := StartSidecar(context.Background(), helperConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sc.Close() }()
	got, err := sc.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []ModelInfo{{Value: "opus", DisplayName: "Opus", Description: "Best"}, {Value: "haiku", DisplayName: "Haiku", Description: "Fast"}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("models = %+v, want %+v", got, want)
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
