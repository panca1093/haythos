package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/panca/haythos/adapters/config"
	"github.com/panca/haythos/adapters/model/chatgpt"
	"github.com/panca/haythos/adapters/model/claudesdk"
	"github.com/panca/haythos/adapters/vault/file"
	"github.com/panca/haythos/app"
	"github.com/panca/haythos/core/policy"
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
		{"no token in output", "1\n", []string{"Login ok\n"}, nil, "haythos auth claude", false},
		{"token wrapped across lines", "1\n", []string{"sk-ant-oat01-AbCdEfGhIjKlMnOpQrStUvWxYz\n0123456789_-AbCdEfGhIjKl\n"}, nil, "haythos auth claude", false},
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
	f.deps.listModels = func(context.Context, string) ([]claudesdk.ModelInfo, error) {
		return nil, errors.New("claude cli down")
	}
	err := runSetup(context.Background(), f.deps)
	if err == nil || !strings.Contains(err.Error(), "claude cli down") {
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

func TestClaudeSubSelectorSwitchesWithoutTouchingConfig(t *testing.T) {
	adapter := claudesdk.New(nopTransport{})
	sel := claudeSubSelector{Adapter: adapter, lister: fakeModelLister{{Value: "sonnet", DisplayName: "Sonnet 5.5", Description: "routine"}}}

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
	variant := sel.WithModel("opus").(*claudesdk.Adapter)
	if variant.Model() != "opus" || sel.Current() != "sonnet" {
		t.Fatalf("per-conversation Claude = %q, shared Claude = %q", variant.Model(), sel.Current())
	}
}

func TestBuildModelsChatGPTNeedsSignIn(t *testing.T) {
	v, _ := file.Open(t.TempDir()+"/v.enc", bytes.Repeat([]byte{7}, 32))
	cfg := config.Main{DefaultModel: "gpt-sub", Models: map[string]config.Model{"gpt-sub": {Kind: "chatgpt"}}}
	models, _, err := buildModels(cfg, v)
	if err != nil || models["gpt-sub"] != nil {
		t.Fatalf("not signed in: models=%v err=%v, want skipped", models, err)
	}
	if err := v.Set("chatgpt", "{}"); err != nil {
		t.Fatal(err)
	}
	models, _, err = buildModels(cfg, v)
	if err != nil || models["gpt-sub"] == nil {
		t.Fatalf("signed in: models=%v err=%v", models, err)
	}
}

func TestBuildModelsOffersChatGPTWithoutYAML(t *testing.T) {
	v, err := file.Open(filepath.Join(t.TempDir(), "v.enc"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Main{
		DefaultModel: "claude-sub",
		Models:       map[string]config.Model{"claude-sub": {Kind: "claudesdk"}},
	}
	models, _, err := buildModels(cfg, v)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := models["chatgpt-sub"].(*chatgpt.Adapter)
	if !ok {
		t.Fatalf("chatgpt-sub = %T, want ChatGPT subscription adapter", models["chatgpt-sub"])
	}
	if _, err := m.Step(context.Background(), nil, nil); !errors.Is(err, chatgpt.ErrSignInRequired) {
		t.Fatalf("unsigned-in ChatGPT error = %v, want sign-in instructions", err)
	}
}

func TestChatToolsHaveAPolicyTier(t *testing.T) {
	pol, err := config.LoadPolicy("../../config/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range append(slices.Clone(defaultChat.Tools), discordChatTools...) {
		if _, ok := pol.Tiers[tool]; !ok {
			t.Errorf("chat tool %s has no tier in config/policy.yaml; the gateway would deny it", tool)
		}
	}
}

func TestDiscordPostIsTierTwoAndOnlyForDiscordChat(t *testing.T) {
	pol, err := config.LoadPolicy("../../config/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := pol.Tiers["discord.post"]; got != policy.TierExternalWrite {
		t.Errorf("discord.post tier = %d, want %d", got, policy.TierExternalWrite)
	}
	if !slices.Contains(discordChatTools, "discord.post") || slices.Contains(defaultChat.Tools, "discord.post") {
		t.Error("discord.post must be added to the chat job only when Discord is configured")
	}
}

func TestClaudeCLIKeepsItsStateNextToTheDatabaseOnePerModel(t *testing.T) {
	cfg := config.Main{DB: filepath.Join("var", "haythos.db")}
	a := claudeCLIConfig(cfg, "tok", "claude-sub").StateDir
	b := claudeCLIConfig(cfg, "tok", "claude-other").StateDir
	if want := filepath.Join("var", "claude-cli", "claude-sub"); a != want {
		t.Errorf("StateDir = %q, want %q", a, want)
	}
	if a == b {
		t.Errorf("two model entries share the state dir %q; the second one's start-up cleanup would delete the first one's transcripts", a)
	}
}

func TestSoulAndRuntimeTailReplaceSelfAbout(t *testing.T) {
	pol, err := config.LoadPolicy("../../config/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"self.about", "vmhealth.read"} {
		if _, ok := pol.Tiers[removed]; ok {
			t.Errorf("%s should be removed from config/policy.yaml", removed)
		}
		if slices.Contains(defaultChat.Tools, removed) {
			t.Errorf("%s should be removed from defaultChat.Tools", removed)
		}
	}

	shipped, err := loadSoul("../../config/haythos.yaml", "")
	if err != nil || !strings.Contains(shipped, "Alfred") {
		t.Fatalf("shipped SOUL.md = %q, err = %v", shipped, err)
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "haythos.yaml")
	got, err := loadSoul(cfgPath, "")
	if err != nil || got != defaultSoul {
		t.Fatalf("missing sibling SOUL.md: got %q err %v, want defaultSoul", got, err)
	}

	explicit := filepath.Join(dir, "custom.md")
	if err := os.WriteFile(explicit, []byte("  Custom Alfred soul.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = loadSoul(cfgPath, explicit)
	if err != nil || got != "Custom Alfred soul." {
		t.Fatalf("explicit soul: got %q err %v", got, err)
	}
	if _, err := loadSoul(cfgPath, filepath.Join(dir, "missing.md")); err == nil {
		t.Error("explicit missing soul path was accepted")
	}

	goal := chatGoal("Static soul prefix.", "v1.2.3", map[string]config.Model{"b": {}, "a": {}}, []string{"discord", "tui"})
	if !strings.HasPrefix(goal, "Static soul prefix.\n\nRuntime:\n") {
		t.Errorf("goal prefix wrong:\n%s", goal)
	}
	for _, want := range []string{"- Version: v1.2.3", "- Models: a, b", "- Channels: discord, tui"} {
		if !strings.Contains(goal, want) {
			t.Errorf("goal missing %q:\n%s", want, goal)
		}
	}
	noChan := chatGoal("Soul.", "dev", nil, nil)
	if !strings.Contains(noChan, "- Models: none") || !strings.Contains(noChan, "- Channels: none") {
		t.Errorf("empty runtime goal:\n%s", noChan)
	}
}
