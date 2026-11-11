package app_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/adapters/tools/local"
	fstools "github.com/panca/haythos/adapters/tools/local/fs"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type trustRig struct {
	app *app.App
	dir string
}

func newTrustRig(t *testing.T, turns ...task.AssistantTurn) *trustRig {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"main.go": "package main", "README.md": "# readme"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := local.New()
	fstools.Register(reg, fstools.Config{Home: dir})
	reg.Register(task.ToolSpec{Name: "brain.remember", Write: true, InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, []byte) (task.ToolResult, error) { return task.ToolResult{Content: "saved"}, nil })
	a := app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"fs.list", "fs.read", "brain.remember"}},
		Tools:      reg, Events: st, Ledger: st, Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{},
		Policy: policy.Policy{
			Version: "v1", AutoWhenClean: []string{"brain.remember"},
			Tiers: map[string]policy.Tier{"fs.list": policy.TierRead, "fs.read": policy.TierRead, "brain.remember": policy.TierInternalWrite},
		},
		Clock: time.Now, ApprovalTTL: time.Hour,
	})
	return &trustRig{app: a, dir: dir}
}

func (r *trustRig) say(t *testing.T, text string) task.State {
	t.Helper()
	id, err := r.app.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: "c1", Workdir: r.dir})
	if err != nil {
		t.Fatalf("handle %q: %v", text, err)
	}
	s, err := r.app.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.State
}

func callTool(name, args string) task.AssistantTurn {
	return task.AssistantTurn{Calls: []task.ToolCall{{ID: "c-" + name, Name: name, Args: json.RawMessage(args)}}}
}

func remember() task.AssistantTurn { return callTool("brain.remember", `{"text":"a fact"}`) }

func TestSavingMemoryAfterReadingFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want task.State
	}{
		{"source code is a valid source: no approval", "main.go", task.Done},
		{"a document still asks first", "README.md", task.WaitingApproval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTrustRig(t, callTool("fs.list", `{}`), callTool("fs.read", `{"path":"`+tc.path+`"}`), remember(), task.AssistantTurn{Text: "done"})
			if got := r.say(t, "learn this repo"); got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSavingMemoryInAFollowUpAfterReadingFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		path string
		want task.State
	}{
		{"code read earlier does not stop the save", "main.go", task.Done},
		{"a document read earlier still asks", "README.md", task.WaitingApproval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTrustRig(t, callTool("fs.read", `{"path":"`+tc.path+`"}`), task.AssistantTurn{Text: "read it"}, remember(), task.AssistantTurn{Text: "saved"})
			if got := r.say(t, "read the file"); got != task.Done {
				t.Fatalf("first message ended %s, want done", got)
			}
			if got := r.say(t, "remember what you learned"); got != tc.want {
				t.Fatalf("follow-up state = %s, want %s", got, tc.want)
			}
		})
	}
}
