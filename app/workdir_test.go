package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	return workdirAppWindows(t, nil, n, tools, turns...)
}

func workdirAppWindows(t *testing.T, windows map[string]int, n *testfakes.Notifier, tools *workdirTools, turns ...task.AssistantTurn) *app.App {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"file.read"}, Budget: task.Budget{Tokens: 50000, Steps: 10}},
		Tools:      tools,
		Windows:    windows,
		Events:     st, Ledger: st, Notifier: n, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{"file.read": policy.TierRead}},
		Clock:  func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) },
	})
}

func readCall() task.AssistantTurn {
	return task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "file.read", Args: json.RawMessage(`{"path":"README.md"}`)}}}
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
			tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: "file.read", InputSchema: json.RawMessage(`{"type":"object"}`)})}
			a := workdirApp(t, n, tools, readCall(), task.AssistantTurn{Text: "done"})
			if _, err := a.HandleInput(context.Background(), app.Input{Text: "read it", ReplyTo: tc.replyTo, Workdir: tc.workdir}); err != nil {
				t.Fatalf("handle input: %v", err)
			}
			if len(tools.seen) != 1 || tools.seen[0] != tc.workdir {
				t.Fatalf("workdir seen by tool = %q, want [%q]", tools.seen, tc.workdir)
			}
			got := n.Of(app.NoticeTool)
			if len(got) != 1 || got[0].ReplyTo != tc.replyTo || !strings.Contains(got[0].Text, "file.read") || !strings.Contains(got[0].Text, "README.md") {
				t.Fatalf("tool notices = %+v, want one file.read README.md to %q", got, tc.replyTo)
			}
		})
	}
}

func TestToolActivityIsNotSentForTasksWithoutOrigin(t *testing.T) {
	n := &testfakes.Notifier{}
	tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: "file.read", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	a := workdirApp(t, n, tools, readCall(), task.AssistantTurn{Text: "done"})
	id, err := a.CreateTask(context.Background(), app.Job{Name: "job", Goal: "g", Model: "default", Tools: []string{"file.read"}}, "go")
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

func TestConversationViewCountsMessagesAndToolCalls(t *testing.T) {
	n := &testfakes.Notifier{}
	tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: "file.read", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	a := workdirApp(t, n, tools, readCall(), task.AssistantTurn{Text: "done"}, task.AssistantTurn{Text: "hi"})
	ctx := context.Background()
	for _, text := range []string{"read it", "thanks"} {
		if _, err := a.HandleInput(ctx, app.Input{Text: text, ReplyTo: "tui:abc", Workdir: "/p"}); err != nil {
			t.Fatalf("handle input: %v", err)
		}
	}
	v, err := a.ConversationView(ctx, "tui:abc")
	if err != nil {
		t.Fatal(err)
	}
	if v.UserMessages != 2 || v.ToolCalls != 1 {
		t.Fatalf("counts = %d user, %d tool, want 2 and 1", v.UserMessages, v.ToolCalls)
	}
}

func TestFinalMessageCarriesContextWindowAndCache(t *testing.T) {
	for _, tc := range []struct {
		name       string
		windows    map[string]int
		wantWindow int
	}{
		{"default window", nil, 200000},
		{"configured window", map[string]int{"default": 1000000}, 1000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &testfakes.Notifier{}
			tools := &workdirTools{Tools: testfakes.NewTools()}
			a := workdirAppWindows(t, tc.windows, n, tools, task.AssistantTurn{Text: "hi", Usage: task.Usage{InputTokens: 1200, OutputTokens: 34, CacheReadTokens: 900}})
			if _, err := a.HandleInput(context.Background(), app.Input{Text: "hello", ReplyTo: "tui:abc"}); err != nil {
				t.Fatal(err)
			}
			got := n.Of(app.NoticeMessage)
			if len(got) != 1 || got[0].Context != 1234 || got[0].Window != tc.wantWindow || got[0].Cached != 900 {
				t.Fatalf("message notice = %+v, want context 1234 of window %d with 900 cached", got, tc.wantWindow)
			}
		})
	}
}

func approvalToolApp(t *testing.T, n *testfakes.Notifier, tool string, tier policy.Tier, turns ...task.AssistantTurn) *app.App {
	t.Helper()
	st, err := sqlite.Open(t.TempDir() + "/haythos.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: tool, Write: true, TargetArg: "path", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	return app.New(app.Deps{
		Models:     map[string]app.Model{"default": testfakes.NewModel(turns...)},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{tool}, Budget: task.Budget{Tokens: 50000, Steps: 10}},
		Tools:      tools,
		Events:     st, Ledger: st, Notifier: n, Vault: testfakes.Vault{},
		Policy: policy.Policy{Tiers: map[string]policy.Tier{tool: tier}},
		Clock:  func() time.Time { return time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC) },
	})
}

func TestToolActivityOnlyWhenTheCallActuallyRuns(t *testing.T) {
	writeCall := task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "notes.write", Args: json.RawMessage(`{"path":"a.md"}`)}}}
	for _, tc := range []struct {
		name    string
		tier    policy.Tier
		decide  bool
		approve bool
		want    int
	}{
		{"approved", policy.TierInternalWrite, true, true, 1},
		{"rejected", policy.TierInternalWrite, true, false, 0},
		{"denied by policy", policy.TierForbidden, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &testfakes.Notifier{}
			a := approvalToolApp(t, n, "notes.write", tc.tier, writeCall, task.AssistantTurn{Text: "done"})
			ctx := context.Background()
			id, err := a.HandleInput(ctx, app.Input{Text: "write", ReplyTo: "tui:abc"})
			if err != nil {
				t.Fatalf("handle input: %v", err)
			}
			if got := n.Of(app.NoticeTool); len(got) != 0 {
				t.Fatalf("tool notices before any decision = %+v, want none", got)
			}
			if tc.decide {
				asks := n.Of(app.NoticeApproval)
				if len(asks) != 1 {
					t.Fatalf("approval requests = %d, want 1", len(asks))
				}
				if err := a.HandleApproval(ctx, id, asks[0].Approval.Approval.ID, tc.approve); err != nil {
					t.Fatalf("approval: %v", err)
				}
			}
			if got := n.Of(app.NoticeTool); len(got) != tc.want {
				t.Fatalf("tool notices = %+v, want %d", got, tc.want)
			}
		})
	}
}

func TestToolTargetIsCutByRunes(t *testing.T) {
	n := &testfakes.Notifier{}
	tools := &workdirTools{Tools: testfakes.NewTools(task.ToolSpec{Name: "file.read", InputSchema: json.RawMessage(`{"type":"object"}`)})}
	long, _ := json.Marshal(map[string]string{"path": strings.Repeat("é", 100)})
	a := workdirApp(t, n, tools, task.AssistantTurn{Calls: []task.ToolCall{{ID: "c1", Name: "file.read", Args: long}}}, task.AssistantTurn{Text: "done"})
	if _, err := a.HandleInput(context.Background(), app.Input{Text: "read", ReplyTo: "tui:abc"}); err != nil {
		t.Fatal(err)
	}
	got := n.Of(app.NoticeTool)
	if len(got) != 1 || !utf8.ValidString(got[0].Text) || !strings.HasSuffix(got[0].Text, strings.Repeat("é", 80)+"…") {
		t.Fatalf("notices = %+v, want 80 whole runes then an ellipsis", got)
	}
}
