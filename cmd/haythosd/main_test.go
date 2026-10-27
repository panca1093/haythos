package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/model/claudesdk"
	"github.com/panca/haythos/adapters/store/sqlite"
	"github.com/panca/haythos/adapters/vault/file"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/task"
	"github.com/panca/haythos/internal/config"
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

const fakeToken = "sk-ant-oat01-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCdEfGhIjKl"

type fakeSecrets struct {
	set map[string]string
	err error
}

func (f *fakeSecrets) Set(name, value string) error {
	if f.err != nil {
		return f.err
	}
	if f.set == nil {
		f.set = map[string]string{}
	}
	f.set[name] = value
	return nil
}

var setupModels = []claudesdk.ModelInfo{
	{Value: "default", DisplayName: "Default (recommended)", Description: "Opus 5.5"},
	{Value: "haiku", DisplayName: "Haiku 5.5", Description: "Fastest"},
}

const setupConfig = "# keep me\ndefault_model: claude-api\nmodels:\n  claude-api: { kind: anthropic, model: m, secret: anthropic }\n  claude-sub: { kind: claudesdk }\n"

type setupFixture struct {
	deps      setupDeps
	secrets   *fakeSecrets
	out       *bytes.Buffer
	cfgPath   string
	listedTok *string
}

// cliChunks are written to the runner's stdout one Write at a time, so a token can be split across writes.
func newSetupFixture(t *testing.T, input string, cliChunks []string, cliErr error) *setupFixture {
	t.Helper()
	f := &setupFixture{secrets: &fakeSecrets{}, out: &bytes.Buffer{}, listedTok: new(string)}
	f.cfgPath = filepath.Join(t.TempDir(), "haythos.yaml")
	if err := os.WriteFile(f.cfgPath, []byte(setupConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	f.deps = setupDeps{
		vault:   f.secrets,
		in:      strings.NewReader(input),
		out:     f.out,
		cfgPath: f.cfgPath,
		runCLI: func(_ context.Context, stdout io.Writer) error {
			for _, c := range cliChunks {
				_, _ = io.WriteString(stdout, c)
			}
			return cliErr
		},
		listModels: func(_ context.Context, token string) ([]claudesdk.ModelInfo, error) {
			*f.listedTok = token
			return setupModels, nil
		},
	}
	return f
}

func happyChunks() []string {
	half := len(fakeToken) / 2
	return []string{"Opening browser...\nPaste code here if prompted > ", "\nYour token:\n\x1b[32m", fakeToken[:half], fakeToken[half:], "\x1b[0m\nStore it securely.\n"}
}

func TestSetupStoresTokenListsModelsAndSavesChoice(t *testing.T) {
	f := newSetupFixture(t, "2\n", happyChunks(), nil)
	if err := runSetup(context.Background(), f.deps); err != nil {
		t.Fatal(err)
	}
	if f.secrets.set["claude"] != fakeToken {
		t.Errorf("vault claude = %q", f.secrets.set["claude"])
	}
	if *f.listedTok != fakeToken {
		t.Errorf("listModels got token %q", *f.listedTok)
	}
	out := f.out.String()
	if strings.Contains(out, fakeToken[:30]) || strings.Contains(out, fakeToken[len(fakeToken)-20:]) {
		t.Errorf("token reached the output:\n%s", out)
	}
	for _, want := range []string{"Opening browser...", "Paste code here if prompted > ", "[stored in vault]", "Store it securely.", "1) Default (recommended)", "2) Haiku 5.5", "claude-sub", "haiku"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	m, err := config.LoadMain(f.cfgPath)
	if err != nil || m.DefaultModel != "claude-sub" || m.Models["claude-sub"].Model != "haiku" {
		t.Errorf("cfg=%+v err=%v", m, err)
	}
}

func TestSetupFailuresStoreNothingMoreAndNeverLeakTheToken(t *testing.T) {
	for _, tc := range []struct {
		name       string
		input      string
		chunks     []string
		cliErr     error
		wantErr    string
		wantStored bool
	}{
		{"no token in output", "1\n", []string{"Login ok\n"}, nil, "haythosd auth claude", false},
		{"token wrapped across lines", "1\n", []string{"sk-ant-oat01-AbCdEfGhIjKlMnOpQrStUvWxYz\n0123456789_-AbCdEfGhIjKl\n"}, nil, "haythosd auth claude", false},
		{"cli fails", "1\n", happyChunks(), errors.New("exit status 1"), "claude setup-token", false},
		{"no model chosen before EOF", "", happyChunks(), nil, "no model chosen", true},
		{"out of range forever", "0\n9\nx\n", happyChunks(), nil, "no model chosen", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupFixture(t, tc.input, tc.chunks, tc.cliErr)
			err := runSetup(context.Background(), f.deps)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), fakeToken[:30]) || strings.Contains(f.out.String(), fakeToken[:30]) {
				t.Error("token leaked")
			}
			if _, stored := f.secrets.set["claude"]; stored != tc.wantStored {
				t.Errorf("stored = %v, want %v", stored, tc.wantStored)
			}
			if raw, _ := os.ReadFile(f.cfgPath); string(raw) != setupConfig {
				t.Errorf("config changed:\n%s", raw)
			}
		})
	}
}

func TestSetupRepromptsOnInvalidChoice(t *testing.T) {
	f := newSetupFixture(t, "abc\n7\n1\n", happyChunks(), nil)
	if err := runSetup(context.Background(), f.deps); err != nil {
		t.Fatal(err)
	}
	if m, _ := config.LoadMain(f.cfgPath); m.Models["claude-sub"].Model != "default" {
		t.Errorf("model = %q", m.Models["claude-sub"].Model)
	}
}

func TestSetupKeepsStoredTokenWhenModelListFails(t *testing.T) {
	f := newSetupFixture(t, "1\n", happyChunks(), nil)
	f.deps.listModels = func(context.Context, string) ([]claudesdk.ModelInfo, error) { return nil, errors.New("sidecar down") }
	err := runSetup(context.Background(), f.deps)
	if err == nil || !strings.Contains(err.Error(), "sidecar down") {
		t.Fatalf("err = %v", err)
	}
	if f.secrets.set["claude"] != fakeToken {
		t.Error("token should stay stored when only the model list fails")
	}
}

func TestSetupRefusesEmptyModelList(t *testing.T) {
	f := newSetupFixture(t, "1\n", happyChunks(), nil)
	f.deps.listModels = func(context.Context, string) ([]claudesdk.ModelInfo, error) { return nil, nil }
	if err := runSetup(context.Background(), f.deps); err == nil {
		t.Error("want error for empty model list")
	}
}

func TestExtractToken(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"plain", "token: " + fakeToken + "\n", fakeToken},
		{"ansi around", "\x1b[1m\x1b[32m" + fakeToken + "\x1b[39m\x1b[22m\n", fakeToken},
		{"crlf", "x\r\n" + fakeToken + "\r\nDone.\r\n", fakeToken},
		{"other prefix digits", "sk-ant-oat02-" + strings.Repeat("a", 40), "sk-ant-oat02-" + strings.Repeat("a", 40)},
		{"placeholder in help text", "paste a token like sk-ant-oat01-... here\n", ""},
		{"nothing", "all good\n", ""},
		{"wrapped", "sk-ant-oat01-" + strings.Repeat("a", 40) + "\n" + strings.Repeat("b", 30) + "\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractToken(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedactorPassesPromptsThroughImmediatelyAndHidesSplitTokens(t *testing.T) {
	var out bytes.Buffer
	r := newRedactor(&out)
	_, _ = io.WriteString(r, "Paste code > ")
	if out.String() != "Paste code > " {
		t.Errorf("prompt held back: %q", out.String())
	}
	_, _ = io.WriteString(r, "tok sk-ant-oat01-AAAA")
	_, _ = io.WriteString(r, "BBBBCCCCDDDDEEEEFFFF\nnext line\n")
	_ = r.Flush()
	if got, want := out.String(), "Paste code > tok [stored in vault]\nnext line\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactorHidesWrappedContinuationAndFlushesTail(t *testing.T) {
	var out bytes.Buffer
	r := newRedactor(&out)
	_, _ = io.WriteString(r, "sk-ant-oat01-AAAABBBBCCCCDDDDEEEE\nFFFFGGGGHHHH\nthanks\ntail-no-newline")
	_ = r.Flush()
	if got, want := out.String(), "[stored in vault]\n[stored in vault]\nthanks\ntail-no-newline"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

type fakeModelLister []claudesdk.ModelInfo

func (f fakeModelLister) Models(context.Context) ([]claudesdk.ModelInfo, error) { return f, nil }

type nopTransport struct{}

func (nopTransport) Step(context.Context, claudesdk.StepRequest) (claudesdk.StepResponse, error) {
	return claudesdk.StepResponse{}, nil
}

func TestClaudeSubSelectorSavesAndSwitches(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "haythos.yaml")
	if err := os.WriteFile(cfgPath, []byte("# keep me\ndefault_model: claude-sub\nmodels:\n  claude-sub: { kind: claudesdk }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := claudesdk.New(nopTransport{})
	sel := claudeSubSelector{adapter: adapter, lister: fakeModelLister{{Value: "sonnet", DisplayName: "Sonnet 5.5", Description: "routine"}}, cfgPath: cfgPath, name: "claude-sub"}

	if got := sel.Current(); got != "default" {
		t.Errorf("Current with no model = %q, want default", got)
	}
	list, err := sel.List(context.Background())
	if err != nil || len(list) != 1 || list[0] != (app.ModelChoice{Value: "sonnet", Name: "Sonnet 5.5", Description: "routine"}) {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := sel.Select(context.Background(), "sonnet"); err != nil {
		t.Fatal(err)
	}
	if adapter.Model() != "sonnet" || sel.Current() != "sonnet" {
		t.Errorf("adapter model = %q, want sonnet", adapter.Model())
	}
	b, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(b), "model: sonnet") || !strings.Contains(string(b), "# keep me") {
		t.Errorf("config not saved with comments kept:\n%s", b)
	}
}

func TestClaudeSubSelectorKeepsModelWhenSaveFails(t *testing.T) {
	adapter := claudesdk.New(nopTransport{}, claudesdk.WithModel("haiku"))
	sel := claudeSubSelector{adapter: adapter, cfgPath: filepath.Join(t.TempDir(), "missing.yaml"), name: "claude-sub"}
	if err := sel.Select(context.Background(), "sonnet"); err == nil {
		t.Fatal("select succeeded without a config file")
	}
	if adapter.Model() != "haiku" {
		t.Errorf("adapter model = %q, want haiku unchanged", adapter.Model())
	}
}
