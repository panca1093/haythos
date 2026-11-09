package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/panca/haythos/adapters/channel/discord"
	"github.com/panca/haythos/adapters/config"
	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func TestBrainFirstOncePerTask(t *testing.T) {
	items := []app.BrainItem{
		{Slug: "a", Title: "A", Text: "alpha", Source: "import/mybook"},
		{Slug: "b", Title: "B", Text: "beta", Source: "owner/thread-1"},
		{Slug: "c", Title: "C", Text: "gamma", Source: "auto/thread-2", Unverified: true},
		{Slug: "d", Title: "D", Text: "delta", Source: "import/mybook"},
		{Slug: "e", Title: "E", Text: "epsilon", Source: "import/mybook"},
	}
	t.Run("hits are injected once, as one block of at most 3", func(t *testing.T) {
		r := newP2Rig(t, items, nil, task.AssistantTurn{Calls: []task.ToolCall{call("c1", "vm.read", `{}`)}}, done("ok"))
		r.say(t, "what do I know about greek letters")
		if len(r.model.Histories) != 2 {
			t.Fatalf("model steps = %d, want 2", len(r.model.Histories))
		}
		for i, h := range r.model.Histories {
			blocks := p2Blocks(h)
			if len(blocks) != 1 {
				t.Fatalf("step %d has %d recalled blocks, want 1", i, len(blocks))
			}
			if n := strings.Count(blocks[0].Text, "<item "); n != 3 {
				t.Fatalf("step %d block has %d items, want 3:\n%s", i, n, blocks[0].Text)
			}
			if !strings.Contains(blocks[0].Text, "import/mybook") || !strings.Contains(blocks[0].Text, "owner/thread-1") {
				t.Fatalf("block lacks source labels:\n%s", blocks[0].Text)
			}
		}
		if got := r.brain.Queries; len(got) != 1 || got[0] != "what do I know about greek letters" || r.brain.Limits[0] != 3 {
			t.Fatalf("recall calls = %v limits %v, want one with the task input and limit 3", got, r.brain.Limits)
		}
	})
	t.Run("no hits injects nothing", func(t *testing.T) {
		r := newP2Rig(t, nil, nil, done("ok"))
		r.say(t, "hello")
		for i, h := range r.model.Histories {
			if len(p2Blocks(h)) != 0 {
				t.Fatalf("step %d has a recalled block with no hits", i)
			}
		}
	})
}

func TestRememberSourceIsKernelStamped(t *testing.T) {
	r := newP2Rig(t, nil, nil,
		task.AssistantTurn{Calls: []task.ToolCall{call("c1", "brain.remember", `{"text":"rapat jam 3","source":"owner/evil"}`)}}, done("noted"))
	r.say(t, "ingat: rapat jam 3")
	if n := len(r.notifier.Of(app.NoticeApproval)); n != 0 {
		t.Fatalf("approval notices = %d, want 0 for an untainted owner chat", n)
	}
	if len(r.brain.Remembered) != 1 {
		t.Fatalf("remembered = %+v, want one item", r.brain.Remembered)
	}
	got := r.brain.Remembered[0]
	if got.Source != "owner/thread-42" || got.Unverified || got.Text != "rapat jam 3" {
		t.Fatalf("stored item = %+v, want source owner/thread-42, verified, text intact", got)
	}
}

var (
	p3WIB   = time.FixedZone("WIB", 7*3600)
	p3Topic = regexp.MustCompile(`topic[a-z]+`)
	p3Slug  = regexp.MustCompile(`inbox/topic[a-z]+`)
)

func p3Day(day, hour, min int) time.Time { return time.Date(2026, 10, day, hour, min, 0, 0, p3WIB) }

// p3Model plays three roles, told apart by the tools it is offered: chat (web.fetch), extraction (brain.remember without
// brain.forget) and consolidation (brain.forget). Extraction saves inbox/<first topicXXX word of its input>; consolidation
// answers one JSON line per inbox slug in its input, reporting topicbeta as merged and everything else as saved.
type p3Model struct {
	mu                sync.Mutex
	extractRuns       int
	extractTools      []string
	consolidateRuns   int
	consolidateInputs []string
}

func (m *p3Model) Step(_ context.Context, history []task.Message, tools []task.ToolSpec) (task.AssistantTurn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for _, t := range tools {
		names = append(names, t.Name)
	}
	last := history[len(history)-1]
	switch {
	case slices.Contains(names, "brain.forget"):
		m.consolidateRuns++
		input := last.Text
		m.consolidateInputs = append(m.consolidateInputs, input)
		var lines []string
		for _, slug := range slices.Compact(p3Slug.FindAllString(input, -1)) {
			if strings.Contains(slug, "topicbeta") {
				lines = append(lines, fmt.Sprintf(`{"slug":%q,"outcome":"merged","into":"entities/people"}`, slug))
			} else {
				lines = append(lines, fmt.Sprintf(`{"slug":%q,"outcome":"saved"}`, slug))
			}
		}
		return task.AssistantTurn{Text: strings.Join(lines, "\n")}, nil
	case slices.Contains(names, "brain.remember"):
		if last.Role != task.RoleUser {
			return task.AssistantTurn{Text: "done"}, nil
		}
		m.extractRuns++
		m.extractTools = names
		var all strings.Builder
		for _, h := range history {
			all.WriteString(h.Text + "\n")
		}
		args, _ := json.Marshal(map[string]string{"slug": "inbox/" + p3Topic.FindString(all.String()), "title": "t", "text": "a decision"})
		return task.AssistantTurn{Calls: []task.ToolCall{{ID: "r1", Name: "brain.remember", Args: args}}}, nil
	case last.Role == task.RoleUser && strings.HasPrefix(last.Text, "read"):
		return task.AssistantTurn{Calls: []task.ToolCall{{ID: "f1", Name: "web.fetch", Args: json.RawMessage(`{}`)}}}, nil
	}
	return task.AssistantTurn{Text: "ok"}, nil
}

type p3Rig struct {
	t        *testing.T
	db       string
	clock    *testfakes.Clock
	model    *p3Model
	brain    *testfakes.Brain
	notifier *testfakes.Notifier
	store    *sqlite.Store
	app      *app.App
}

func newP3Rig(t *testing.T) *p3Rig {
	t.Helper()
	r := &p3Rig{
		t: t, db: t.TempDir() + "/haythos.db", clock: testfakes.NewClock(p3Day(27, 9, 0)),
		model: &p3Model{}, brain: testfakes.NewBrain(), notifier: &testfakes.Notifier{},
	}
	r.boot()
	r.tick()
	return r
}

func (r *p3Rig) boot() {
	r.t.Helper()
	st, err := sqlite.Open(r.db)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.t.Cleanup(func() { _ = st.Close() })
	r.store = st
	all, err := config.LoadJobs("../../jobs")
	if err != nil {
		r.t.Fatalf("load jobs: %v", err)
	}
	var jobs []app.Job
	for _, j := range all {
		if strings.HasPrefix(j.Name, "brain-") {
			jobs = append(jobs, j)
		}
	}
	if len(jobs) != 2 {
		r.t.Fatalf("brain jobs = %d, want brain-consolidate and brain-digest", len(jobs))
	}
	tools := testfakes.NewBrainTools(r.brain, task.ToolSpec{Name: "web.fetch", InputSchema: json.RawMessage(`{"type":"object"}`), Untrusted: true})
	r.app = app.New(app.Deps{
		Models:     map[string]app.Model{"default": r.model},
		DefaultJob: app.Job{Name: "chat", Goal: "You are Alfred.", Model: "default", Tools: []string{"web.fetch"}, Budget: task.Budget{Tokens: 1000000, Steps: 20}},
		Jobs:       jobs, Tools: tools, Events: st, Ledger: st, Notifier: r.notifier, Vault: testfakes.Vault{}, Brain: r.brain,
		Policy: policy.Policy{Version: "v1", Tiers: map[string]policy.Tier{
			"web.fetch": policy.TierRead, "brain.recall": policy.TierRead, "brain.remember": policy.TierRead, "brain.forget": policy.TierRead,
		}},
		Clock: r.clock.Now, ApprovalTTL: time.Hour,
	})
	if err := r.app.Start(context.Background()); err != nil {
		r.t.Fatalf("start: %v", err)
	}
}

func (r *p3Rig) tick() {
	r.t.Helper()
	if err := r.app.Tick(context.Background()); err != nil {
		r.t.Fatalf("tick: %v", err)
	}
}

func (r *p3Rig) at(t time.Time) {
	r.t.Helper()
	if t.Before(r.clock.Now()) {
		r.t.Fatalf("clock cannot go back to %s", t)
	}
	r.clock.Advance(t.Sub(r.clock.Now()))
}

func (r *p3Rig) send(replyTo, text string) {
	r.t.Helper()
	if _, err := r.app.HandleInput(context.Background(), app.Input{Text: text, ReplyTo: replyTo}); err != nil {
		r.t.Fatalf("handle input %q: %v", text, err)
	}
}

func (r *p3Rig) extractRuns() int {
	r.model.mu.Lock()
	defer r.model.mu.Unlock()
	return r.model.extractRuns
}

// backgroundNotices are notices that did not come from a chat task or a digest.
func (r *p3Rig) backgroundNotices() []app.Notice {
	var out []app.Notice
	for _, n := range r.notifier.Notices {
		if !strings.HasPrefix(n.TaskID, "chat-") && n.Kind != app.NoticeDigest {
			out = append(out, n)
		}
	}
	return out
}

func (r *p3Rig) eventCount(typ task.EventType) int {
	r.t.Helper()
	evs, err := r.store.Since(context.Background(), 0)
	if err != nil {
		r.t.Fatalf("read events: %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// P3-1. Idle 3 h with new turns starts exactly one extraction, also across a restart; less than 3 h starts none.
func TestBrainStaleConversationStartsOneExtraction(t *testing.T) {
	r := newP3Rig(t)
	r.send("thread-1", "topicalpha we decided to use sqlite")

	r.at(p3Day(27, 11, 59))
	r.tick()
	if got := r.extractRuns(); got != 0 {
		t.Fatalf("extractions after 2h59 idle = %d, want 0", got)
	}
	r.at(p3Day(27, 12, 0))
	r.tick()
	if got := r.extractRuns(); got != 1 {
		t.Fatalf("extractions after 3h idle = %d, want 1", got)
	}
	r.tick()
	r.boot()
	r.tick()
	if got := r.extractRuns(); got != 1 {
		t.Fatalf("extractions after more ticks and a restart = %d, want still 1", got)
	}
	if n := r.backgroundNotices(); len(n) != 0 {
		t.Fatalf("extraction sent notices: %+v", n)
	}

	r.send("thread-1", "topicgamma another decision")
	r.at(p3Day(27, 15, 0))
	r.tick()
	r.boot()
	r.tick()
	if got := r.extractRuns(); got != 2 {
		t.Fatalf("extractions after a second idle period = %d, want 2", got)
	}
}

// P3-2. A conversation that was ever tainted is not extracted; the digest reports it as skipped.
func TestBrainTaintedConversationIsSkippedAndReported(t *testing.T) {
	r := newP3Rig(t)
	r.send("thread-tainted", "read https://example.invalid/page topictaint")
	r.send("thread-clean", "topicclean we prefer small diffs")

	r.at(p3Day(27, 12, 30))
	r.tick()
	if got := r.extractRuns(); got != 1 {
		t.Fatalf("extractions = %d, want 1 (only the clean conversation)", got)
	}
	if _, ok := r.brain.Item("inbox/topictaint"); ok {
		t.Fatal("tainted conversation reached the brain")
	}

	r.at(p3Day(28, 2, 0))
	r.tick()
	r.at(p3Day(28, 7, 0))
	r.tick()
	digests := r.notifier.Of(app.NoticeDigest)
	if len(digests) != 1 {
		t.Fatalf("digests = %d, want 1", len(digests))
	}
	if !strings.Contains(digests[0].Text, "thread-tainted") {
		t.Errorf("digest does not report the skipped conversation:\n%s", digests[0].Text)
	}
}

// P3-3. Extracted items land in the inbox, stamped auto/<conversation> and unverified, by a task that only has recall and remember.
func TestBrainExtractedItemsAreInboxUnverifiedAutoSource(t *testing.T) {
	r := newP3Rig(t)
	r.send("thread-1", "topicalpha we decided to use sqlite")
	r.at(p3Day(27, 12, 0))
	r.tick()

	it, ok := r.brain.Item("inbox/topicalpha")
	if !ok {
		t.Fatalf("no inbox item; brain = %+v", r.brain.Items)
	}
	if it.Source != "auto/thread-1" || !it.Unverified {
		t.Errorf("item = %+v, want source auto/thread-1 and unverified", it)
	}
	got := slices.Clone(r.model.extractTools)
	slices.Sort(got)
	if !slices.Equal(got, []string{"brain.recall", "brain.remember"}) {
		t.Errorf("extraction tools = %v, want only brain.recall and brain.remember", got)
	}
}

// P3-4. Consolidation sees only items saved on the previous Jakarta day (not today's, even when it is still the previous UTC day) and sends no notice.
func TestBrainConsolidateTouchesOnlyPreviousJakartaDay(t *testing.T) {
	r := newP3Rig(t)
	r.send("thread-a", "topicalpha first decision")
	r.at(p3Day(27, 12, 0))
	r.tick()

	r.at(p3Day(27, 22, 0))
	r.send("thread-b", "topicbeta second decision")
	r.at(p3Day(28, 1, 0))
	r.tick()
	if _, ok := r.brain.Item("inbox/topicbeta"); !ok {
		t.Fatal("setup: topicbeta was not extracted")
	}

	r.at(p3Day(28, 2, 0))
	r.tick()
	if r.model.consolidateRuns != 1 {
		t.Fatalf("consolidation runs = %d, want 1", r.model.consolidateRuns)
	}
	in := r.model.consolidateInputs[0]
	if !strings.Contains(in, "inbox/topicalpha") || strings.Contains(in, "inbox/topicbeta") {
		t.Errorf("consolidation input must hold only the previous day's item:\n%s", in)
	}
	if n := r.backgroundNotices(); len(n) != 0 {
		t.Errorf("consolidation sent notices: %+v", n)
	}

	r.at(p3Day(29, 2, 0))
	r.tick()
	if r.model.consolidateRuns != 2 {
		t.Fatalf("consolidation runs = %d, want 2", r.model.consolidateRuns)
	}
	in = r.model.consolidateInputs[1]
	if !strings.Contains(in, "inbox/topicbeta") || strings.Contains(in, "inbox/topicalpha") {
		t.Errorf("second consolidation must hold only topicbeta:\n%s", in)
	}
}

// P3-5. The digest lists saved and merged items once, with an Undo each; nothing to report sends nothing; Undo forgets once and is recorded.
func TestBrainDigestListsItemsWithUndoAndNothingOtherwise(t *testing.T) {
	r := newP3Rig(t)
	r.send("thread-a", "topicalpha first decision")
	r.at(p3Day(27, 9, 10))
	r.send("thread-b", "topicbeta second decision")
	r.at(p3Day(27, 12, 30))
	r.tick()
	r.at(p3Day(28, 2, 0))
	r.tick()
	r.at(p3Day(28, 7, 0))
	r.tick()

	digests := r.notifier.Of(app.NoticeDigest)
	if len(digests) != 1 {
		t.Fatalf("digests = %d, want 1", len(digests))
	}
	d := digests[0]
	for _, want := range []string{"inbox/topicalpha", "inbox/topicbeta", "entities/people"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("digest is missing %q:\n%s", want, d.Text)
		}
	}
	if len(d.Undo) != 2 {
		t.Fatalf("undo buttons = %d, want one per item (2)", len(d.Undo))
	}
	if n := r.backgroundNotices(); len(n) != 0 {
		t.Errorf("background notices = %+v, want none", n)
	}

	r.at(p3Day(28, 7, 1))
	r.tick()
	r.at(p3Day(29, 7, 0))
	r.tick()
	if got := len(r.notifier.Of(app.NoticeDigest)); got != 1 {
		t.Fatalf("digests after a day with nothing new = %d, want still 1", got)
	}

	ctx := context.Background()
	before := r.brain.ForgetCalls()
	if err := r.app.UndoBrain(ctx, d.Undo[0].ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if got := r.brain.ForgetCalls() - before; got != 1 {
		t.Fatalf("forget calls after undo = %d, want 1", got)
	}
	if last := r.brain.Forgotten[len(r.brain.Forgotten)-1]; !strings.HasPrefix(last, "inbox/topic") {
		t.Errorf("forgot %q, want an inbox item", last)
	}
	if err := r.app.UndoBrain(ctx, d.Undo[0].ID); err != nil {
		t.Fatalf("second undo must be harmless: %v", err)
	}
	if got := r.brain.ForgetCalls() - before; got != 1 {
		t.Errorf("forget calls after double click = %d, want still 1", got)
	}
	if got := r.eventCount("brain.undone"); got != 1 {
		t.Errorf("brain.undone events = %d, want 1", got)
	}
}

// P3-6. A tainted task's notice is marked tainted and rendered with embeds suppressed; a clean one is not.
func TestBrainTaintedNoticeSuppressesEmbeds(t *testing.T) {
	r := newP3Rig(t)
	r.send("thread-tainted", "read https://example.invalid/page")
	r.send("thread-clean", "plain question")

	finals := func(replyTo string) app.Notice {
		t.Helper()
		for _, n := range r.notifier.Of(app.NoticeMessage) {
			if n.ReplyTo == replyTo {
				return n
			}
		}
		t.Fatalf("no message notice for %s", replyTo)
		return app.Notice{}
	}
	suppressed := func(n app.Notice) bool {
		msgs := discord.Render(n)
		if len(msgs) == 0 {
			t.Fatal("render produced no messages")
		}
		for _, m := range msgs {
			if m.Flags&discordgo.MessageFlagsSuppressEmbeds == 0 {
				return false
			}
		}
		return true
	}
	tainted, clean := finals("thread-tainted"), finals("thread-clean")
	if !tainted.Tainted || clean.Tainted {
		t.Fatalf("Tainted = %v/%v, want true for the fetch task and false for the plain one", tainted.Tainted, clean.Tainted)
	}
	if !suppressed(tainted) {
		t.Error("tainted notice was rendered without SuppressEmbeds")
	}
	if suppressed(clean) {
		t.Error("clean notice was rendered with SuppressEmbeds")
	}
}
