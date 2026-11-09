package acceptance_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/testfakes"
	"github.com/panca/haythos/adapters/tools/local"
	"github.com/panca/haythos/adapters/tools/local/vmhealth"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
	"github.com/panca/haythos/core/task"
)

func TestVMHealthTwoScheduledRunsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	jakarta := time.FixedZone("WIB", 7*3600)
	clock := testfakes.NewClock(time.Date(2026, 10, 27, 6, 0, 0, 0, jakarta))
	db := t.TempDir() + "/h.db"
	notifier := &testfakes.Notifier{}

	diskPct := "40"
	collector := vmhealth.Collector{
		NumCPU:   4,
		ReadFile: func(p string) ([]byte, error) { return readProc(p) },
		Run: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			switch name {
			case "df":
				return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 1000 500 500 " + diskPct + "% /\n"), nil
			case "ps":
				return []byte("%CPU %MEM COMMAND\n 1.0 2.0 sshd\n"), nil
			}
			return nil, errors.New("absent")
		},
	}
	reg := local.New()
	vmhealth.Register(reg, collector)

	job := app.Job{
		Name: "vm-health", Goal: "Summarize anomalies from the VM health data.", Model: "default",
		Tools:     []string{"vmhealth.read"},
		Budget:    task.Budget{Tokens: 20000, Steps: 5},
		Trigger:   app.Trigger{Cron: "CRON_TZ=Asia/Jakarta 0 7,15 * * *"},
		Collect:   "vmhealth.read",
		NormalMsg: "VM health: all normal",
	}
	var crashAt string
	boot := func(turns ...task.AssistantTurn) (*app.App, *sqlite.Store, *testfakes.Model) {
		st, err := sqlite.Open(db)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		m := testfakes.NewModel(turns...)
		return app.New(app.Deps{
			Models: map[string]app.Model{"default": m}, DefaultJob: testJob, Jobs: []app.Job{job},
			Tools: reg, Events: st, Ledger: st, Notifier: notifier, Vault: testfakes.Vault{},
			Policy: policy.Policy{Version: "v1", Tiers: map[string]policy.Tier{"vmhealth.read": policy.TierRead}},
			Clock:  clock.Now,
			Hook: func(p string) error {
				if p == crashAt {
					crashAt = ""
					return errCrash
				}
				return nil
			},
		}), st, m
	}
	reports := func() []string {
		var out []string
		for _, n := range notifier.Of(app.NoticeMessage) {
			out = append(out, n.Text)
		}
		return out
	}

	a, st, _ := boot()
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	clock.Advance(time.Hour + 30*time.Second)
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := reports(); len(got) != 1 || got[0] != "VM health: all normal" {
		t.Fatalf("after morning run reports = %q", got)
	}

	_ = st.Close()
	diskPct = "95"
	clock.Advance(8 * time.Hour)
	a, st, _ = boot(finalTurn("Disk / is 95% full. Free space or extend the volume."))
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	crashAt = "app.after_tool_result"
	if err := a.Tick(ctx); !errors.Is(err, errCrash) {
		t.Fatalf("afternoon tick: want crash, got %v", err)
	}
	if got := len(reports()); got != 1 {
		t.Fatalf("report leaked before crash recovery: %d", got)
	}

	_ = st.Close()
	a, _, model := boot(finalTurn("Disk / is 95% full. Free space or extend the volume."))
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := reports()
	if len(got) != 2 || !strings.Contains(got[1], "95%") {
		t.Fatalf("reports = %q, want exactly two (normal, then disk alert)", got)
	}
	if model.Steps != 1 {
		t.Errorf("model steps = %d, want 1 (collect runs in Go, model only summarizes)", model.Steps)
	}
}

func readProc(p string) ([]byte, error) {
	switch p {
	case "/proc/loadavg":
		return []byte("0.5 0.5 0.5 1/10 1\n"), nil
	case "/proc/meminfo":
		return []byte("MemTotal: 1000 kB\nMemAvailable: 600 kB\n"), nil
	}
	return nil, errors.New("absent")
}
