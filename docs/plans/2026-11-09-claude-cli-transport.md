# Claude CLI Transport (drop the Node sidecar) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `claude-sub` calls the official `claude` CLI directly from Go; `sidecar/claude-sdk` (Node) is deleted.

**Architecture:** A new `claudesdk.CLI` implements the existing `claudesdk.Transport` (`Step`) and the model lister (`Models`) by spawning `claude -p` per call. `claudesdk.Adapter`, `parseOutput`, the repair retry and the JSON-schema contract stay as they are, so the frozen M1 contract test keeps passing unchanged. Wiring in `cmd/haythosd` swaps `StartSidecar` for `NewCLI`.

**Tech Stack:** Go stdlib (`os/exec`, `encoding/json`, `bufio`). No new dependency.

**Spec:** `experiments/FINDINGS.md` §5 (CLI flags, latency, model list handshake); `CLAUDE.md` rules 1, 3, 5.

## Global Constraints

- Frozen: `tests/acceptance/task_lifecycle_test.go` must pass with zero diff. Do not rename package `claudesdk` or change `New`, `Transport`, `StepRequest`, `StepResponse`, `ErrInvalidOutput`, `ModelInfo`.
- Config `kind: claudesdk` keeps its name (existing configs and the contract test use it).
- Child env is never inherited: exactly `PATH`, `HOME`, `CLAUDE_CODE_OAUTH_TOKEN` (same as `sidecarConfig` today) plus the fixed flags below.
- Step argv (fixed): `-p --output-format json --tools "" --setting-sources "" --no-session-persistence --strict-mcp-config --max-turns 2 --system-prompt <system> --json-schema <schema>` plus `--model <m>` when set. Prompt goes on stdin. `cmd.Dir` = a fresh `os.MkdirTemp` dir, removed after the call.
- Extra env (fixed): `CLAUDE_CODE_DISABLE_AUTO_MEMORY=1`, `CLAUDE_CODE_DISABLE_CLAUDE_MDS=1`, `CLAUDE_CODE_MAX_RETRIES=0`.
- Never `--bare` (it ignores OAuth).
- `make gate` green; `go test -race ./...`.

## Review Focus

- CLI not on PATH or wrong path → `Step` returns a wrapped error naming the command, haythosd still starts (model skipped like a missing credential).
- CLI exits non-zero with `{"is_error":true}` JSON on stdout (expired token, 401) → error carries the CLI's `result` text, never the token.
- `ctx` cancelled mid-call → child process is killed, temp dir removed, `ctx.Err()` returned.
- `structured_output` absent but `result` present (model ignored schema) → returned as `Text`, so the existing repair retry handles it.
- Huge stdout (long answer) → no 64 KB scanner truncation (`cmd.Output()`, not a line scanner, for Step).

---

### Task 1: `CLI.Step` transport

**Files:**
- Create: `adapters/model/claudesdk/cli.go`
- Test: `adapters/model/claudesdk/cli_test.go`

**Interfaces:**
- Produces: `type CLIConfig struct { Command string; Env []string }`, `func NewCLI(cfg CLIConfig) *CLI`, `func (c *CLI) Step(ctx context.Context, req StepRequest) (StepResponse, error)` (satisfies `Transport`).

- [ ] **Step 1: Write failing tests** using a fake CLI: the test binary re-execs itself (`os.Args[0]` with env `CLAUDECLI_FAKE=<mode>`, pattern `TestMain`/helper-process) so no real `claude` runs.
  - `TestCLIStepArgsAndEnv`: fake records argv, env and stdin to a file; assert argv equals the fixed list (with `--model sonnet` when `req.Model=="sonnet"`, absent when empty), env has exactly the allowed keys, stdin == `req.Prompt`, cwd is not the test's cwd.
  - `TestCLIStepParsesStructuredOutput`: fake prints `{"subtype":"success","structured_output":{"text":"hi"},"result":"","usage":{"input_tokens":3,"output_tokens":2}}` → `StepResponse{Text:`{"text":"hi"}`, Usage{3,2}}`.
  - `TestCLIStepFallsBackToResult`: no `structured_output`, `result:"plain"` → `Text=="plain"`.
  - `TestCLIStepErrorResult`: exit 1 + `{"is_error":true,"result":"Invalid API key"}` → error contains `Invalid API key`.
  - `TestCLIStepCancel`: fake sleeps 10 s; cancel ctx after 100 ms → returns within 2 s, `errors.Is(err, context.Canceled)`.
- [ ] **Step 2:** `go test ./adapters/model/claudesdk/ -run CLI` → FAIL (undefined `NewCLI`).
- [ ] **Step 3: Implement** in `cli.go`. `exec.CommandContext`, `cmd.Output()`; decode `{subtype, is_error, result, structured_output json.RawMessage, usage{input_tokens,output_tokens}}`; a non-zero exit with parseable `is_error` JSON reports `result`; otherwise wrap `exec.ExitError` with stderr tail (max 500 bytes).
- [ ] **Step 4:** tests PASS, plus `go test -race ./adapters/model/claudesdk/`.
- [ ] **Step 5: Commit** `claudesdk: add CLI transport that spawns claude -p`

### Task 2: `CLI.Models` via the `initialize` handshake

**Files:**
- Modify: `adapters/model/claudesdk/cli.go`
- Test: `adapters/model/claudesdk/cli_test.go`

**Interfaces:**
- Produces: `func (c *CLI) Models(ctx context.Context) ([]ModelInfo, error)` (satisfies `modelLister` in `cmd/haythosd`).

- [ ] **Step 1: Failing test** `TestCLIModels`: fake (stream-json mode) reads one stdin line, asserts it is `{"type":"control_request","request_id":"models","request":{"subtype":"initialize"}}`, prints a `control_response` line whose `response.response.models` has two entries plus one unrelated `system` line → two `ModelInfo{Value, DisplayName, Description}`; assert argv contains `--input-format stream-json --output-format stream-json --verbose` and no model turn flags beyond Task 1's isolation flags.
  - `TestCLIModelsNoResponse`: fake prints nothing and waits → returns ctx deadline error, child killed.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement**: write the request, scan stdout lines (buffer up to 16 MB) for `type=="control_response"` with `request_id=="models"`, then close stdin and kill the child. Never send a user message. Response path: verify against the real CLI once (`experiments/FINDINGS.md` §5 shows `"models":[...]` inside the initialize response) and pin the struct to it.
- [ ] **Step 4:** PASS.
- [ ] **Step 5: Commit** `claudesdk: list models through the CLI initialize handshake`

### Task 3: Wire CLI into haythosd, delete the sidecar

**Files:**
- Modify: `cmd/haythosd/main.go` (`case "claudesdk"`, `sidecarConfig`, `sidecarModels`, `closers`)
- Modify: `adapters/config/config.go:36-40`, `adapters/config/config_test.go:83-86`
- Modify: `config/haythos.yaml:10-14`, `Makefile:1,15-18`, `README.md` (lines 7, 15, 22, 39, 62, 112, 131)
- Delete: `sidecar/claude-sdk/`, `adapters/model/claudesdk/sidecar.go` and the sidecar parts of `claudesdk_test.go`
- Test: `cmd/haythosd/main_test.go`, `adapters/config/config_test.go`

**Interfaces:**
- Consumes: `NewCLI`, `(*CLI).Step`, `(*CLI).Models` from Tasks 1–2.
- Produces: config `claude_cli: { command: claude }` (`Main.ClaudeCLI.Command string`, default `"claude"` when empty).

- [ ] **Step 1: Failing tests**: config test loads `claude_cli: {command: /opt/claude}` → `cfg.ClaudeCLI.Command=="/opt/claude"`; a config that still has the old `sidecar:` block loads without error and logs a deprecation warning (same pattern as `discord.channel_id`); `main_test.go` model-list error case renamed from "sidecar down" to "claude cli down".
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement**: replace `StartSidecar` with `claudesdk.NewCLI(claudesdk.CLIConfig{Command: cfg.ClaudeCLI.Command, Env: claudeEnv(token)})`; `claudeEnv` returns the three allowed vars; no closer needed (per-call processes). Keep `Sidecar` struct only as a deprecated, ignored field. Delete the sidecar code and dir, drop the `sidecar` Makefile target from `gate`, update README (requirements: `claude` CLI, no Node; setup step 2 text; layout table row removed).
- [ ] **Step 4:** `make gate` → green; `git diff --stat tests/acceptance/` → empty.
- [ ] **Step 5: Commit** `claudesdk: run claude-sub on the CLI and remove the Node sidecar`

### Task 4: Live check (with the user)

- [ ] `haythosd setup` lists models from the CLI; `run-job vm-health` on `claude-sub` with the vault token and scrubbed env returns a report. Record latency of 3 calls and whether `num_turns` stayed ≤ 2 in `docs/REPORT.md` (live checks section).
- [ ] Pin: note the CLI version used; if the user wants a pinned binary, set `claude_cli.command` to an absolute path.

---

**Deferred (separate decision, not in this plan):** native `tool_use` via inert MCP + `--max-turns 1` + stream-json history replay (FINDINGS §5). It changes the adapter's output contract, so it would be a new adapter kind (e.g. `claudenative`) with its own tests, keeping `claudesdk` for the frozen contract test.
