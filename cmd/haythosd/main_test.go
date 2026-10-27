package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/vault/file"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
	"github.com/panca/haythos/internal/testfakes"
)

func TestInstanceLockIsExclusive(t *testing.T) {
	db := filepath.Join(t.TempDir(), "h.db")
	release, err := lockInstance(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockInstance(db); err == nil {
		t.Fatal("second lock on the same db succeeded")
	}
	release()
	release2, err := lockInstance(db)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	release2()
}

func runJobFixture(t *testing.T) (dir string, args []string) {
	t.Helper()
	dir = t.TempDir()
	t.Setenv("HAYTHOS_VAULT_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	cfg := "db: " + filepath.Join(dir, "h.db") + "\nvault: " + filepath.Join(dir, "v.enc") + "\n" +
		"default_model: claude-api\nmodels:\n  claude-api: { kind: anthropic, model: m, secret: anthropic }\n"
	if err := os.WriteFile(filepath.Join(dir, "haythos.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, []string{"-config", filepath.Join(dir, "haythos.yaml"), "-policy", "../../config/policy.yaml", "-jobs", "../../jobs", "run-job", "vm-health"}
}

func TestRunJobDoesNotResumeOtherTasks(t *testing.T) {
	dir, args := runJobFixture(t)
	db := filepath.Join(dir, "h.db")
	st, err := sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(app.Deps{Events: st, Ledger: st, Tools: testfakes.NewTools(), Notifier: &testfakes.Notifier{}, Vault: testfakes.Vault{}})
	other, err := a.CreateTask(context.Background(), app.Job{Name: "chat", Goal: "g"}, "hi from discord")
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	if err := run(args); err != nil {
		t.Fatalf("run-job: %v", err)
	}

	st, err = sqlite.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pending, err := st.TasksIn(context.Background(), task.Pending)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pending, other) {
		t.Errorf("task %s was resumed by run-job; pending = %v", other, pending)
	}
}

func TestRunJobRefusesWhileAnotherInstanceHoldsTheDB(t *testing.T) {
	dir, args := runJobFixture(t)
	release, err := lockInstance(filepath.Join(dir, "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := run(args); err == nil || !strings.Contains(err.Error(), "another haythosd") {
		t.Errorf("err = %v, want another-instance error", err)
	}
}

func TestAuthStoresTrimmedSecretAndNeverEchoesIt(t *testing.T) {
	v, err := file.Open(t.TempDir()+"/v.enc", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runAuth([]string{"claude"}, v, &out, func() (string, error) { return "  tok-123\n", nil }); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Resolve("secret://claude"); got != "tok-123" {
		t.Errorf("stored %q", got)
	}
	if strings.Contains(out.String(), "tok-123") {
		t.Error("output echoes the secret")
	}
}

func TestAuthRejectsBadInput(t *testing.T) {
	v, _ := file.Open(t.TempDir()+"/v.enc", bytes.Repeat([]byte{7}, 32))
	read := func() (string, error) { return " \n", nil }
	var out bytes.Buffer
	if runAuth([]string{"nope"}, v, &out, read) == nil {
		t.Error("unknown target accepted")
	}
	if runAuth([]string{"openai"}, v, &out, read) == nil {
		t.Error("empty secret accepted")
	}
	if runAuth(nil, v, &out, read) == nil {
		t.Error("missing target accepted")
	}
}
