package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

type workdirTools struct {
	*testfakes.Tools
	seen []string
}

func (w *workdirTools) Call(ctx context.Context, call task.ToolCall) (task.ToolResult, error) {
	w.seen = append(w.seen, app.WorkdirFrom(ctx))
	return task.ToolResult{CallID: call.ID, Content: "ok"}, nil
}

func workdirApp(t *testing.T, n *testfakes.Notifier, tools *workdirTools, turns ...task.AssistantTurn) *app.App {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"fs.read"}},
		Tools:      tools,
		Events:     st, Ledger: st, Notifier: n, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{"fs.read": policy.TierRead}},
		Clock:  func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) },
	})
}

func readCall() task.AssistantTurn {
	return task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "fs.read", Args: json.RawMessage(`{"path":"README.md"}`)}}}
}

func TestWorkdirReachesToolsAndToolActivityGoesToOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, replyTo, workdir string
		wantNotice             bool
	}{
		{"tui chat in a folder", "tui:abc", "/home/me/project", true},
		{"discord chat has no folder", "123456", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &testfakes.Notifier{}
			tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: "fs.read", InputSchema: json.RawMessage(`{"type":"object"}`)})}
			a := workdirApp(t, n, tools, readCall(), task.AssistantTurn{Text: "done"})
			if _, err := a.HandleInput(context.Background(), app.Input{Text: "read it", ReplyTo: tc.replyTo, Workdir: tc.workdir}); err != nil {
				t.Fatalf("handle input: %v", err)
			}
			if len(tools.seen) != 1 || tools.seen[0] != tc.workdir {
				t.Fatalf("workdir seen by tool = %q, want [%q]", tools.seen, tc.workdir)
			}
			got := n.Of(app.NoticeTool)
			if len(got) != 1 || got[0].ReplyTo != tc.replyTo || !strings.Contains(got[0].Text, "fs.read") || !strings.Contains(got[0].Text, "README.md") {
				t.Fatalf("tool notices = %+v, want one fs.read README.md to %q", got, tc.replyTo)
			}
		})
	}
}

func TestToolActivityIsNotSentForTasksWithoutOrigin(t *testing.T) {
	n := &testfakes.Notifier{}
	tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: "fs.read", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	a := workdirApp(t, n, tools, readCall(), task.AssistantTurn{Text: "done"})
	id, err := a.CreateTask(context.Background(), app.Job{Name: "job", Goal: "g", Model: "default", Tools: []string{"fs.read"}}, "go")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := a.RunTask(context.Background(), id); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := n.Of(app.NoticeTool); len(got) != 0 {
		t.Fatalf("tool notices = %+v, want none for a task without an origin", got)
	}
}
