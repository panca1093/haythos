# Codex (ChatGPT subscription) Model Adapter Implementation Plan

> **Status 2026-11-10: superseded.** OpenAI's official Sign in with ChatGPT token sharing (FINDINGS §6, as used in the jaze project) gives the same plan access over the public Responses API with our own loop. Kept for reference; do not execute.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `kind: codex` model that runs one Alfred `Step` on the user's ChatGPT plan through the official `codex app-server`, with tools executed only by the HaythOS gateway.

**Architecture:** `adapters/model/codex` spawns `codex app-server` per `Step` (stdio JSON-RPC), starts an ephemeral thread with our system prompt as `baseInstructions` and our tools as `dynamicTools`, replays history with `thread/inject_items`, runs `turn/start` with empty input, and returns the agent text or the first `item/tool/call` (then `turn/interrupt`). The request is never answered, so Codex never sees a tool result it did not get from history.

**Tech Stack:** Go stdlib. No new dependency.

**Spec:** `docs/FINDINGS.md` §6 (protocol, flags, isolation); the spike script was throwaway and is not kept; `CLAUDE.md` rules 1, 2, 3, 5.

## Global Constraints

- Official binary only; never call `chatgpt.com/backend-api` directly, never reuse Codex's OAuth client id, never set a spoofed user-agent/originator. `clientInfo.name` = `"haythos"`.
- `CODEX_HOME` is a HaythOS-owned dir (`codex.home`, default `data/codex`, mode 0700), never `~/.codex`. Child env: `PATH`, `HOME`, `CODEX_HOME` only.
- `thread/start` fixed fields: `ephemeral: true`, `approvalPolicy: "never"`, `sandbox: "read-only"`, `cwd`: fresh temp dir, `config.web_search: "disabled"`, `config.features.<f>: false` for `shell_tool, unified_exec, view_image, image_generation, apps, browser_use, computer_use, skill_search, tool_suggest, sleep_tool`.
- `initialize.capabilities.experimentalApi: true` (dynamic tools need it).
- Tool names on the wire: `.` → `__` (same `wireName/localName` rule as `adapters/model/openai`).
- Any server request other than `item/tool/call` (approval requests = a built-in tool tried to run) → reply with a JSON-RPC error, interrupt, and fail the Step with `ErrBuiltinTool`.
- `make gate` green; `go test -race ./...`.

## Review Focus

- A built-in tool fires despite the feature flags → Step fails with `ErrBuiltinTool`, nothing executes (approval answered with an error, never "approved").
- Model emits two tool calls in one turn → only the first is returned; the second request (if it arrives before interrupt) is answered with an error, not executed.
- Not logged in / expired auth → `initialize` or `turn/start` error surfaces as a wrapped error with the hint `run haythosd auth codex`.
- History with an assistant tool call whose result is missing (crash between dispatch and result) → `inject_items` still gets a matching `function_call_output` with `is_error`-style content, otherwise the Responses API rejects the thread. (Reconcile owns the real fix; the adapter must not crash.)
- `ctx` cancelled → child killed, temp cwd removed.

---

### Task 1: History and tool mapping (pure functions)

**Files:**
- Create: `adapters/model/codex/codex.go`
- Test: `adapters/model/codex/codex_test.go`

**Interfaces:**
- Produces: `func instructions(history []task.Message) string` (joined `RoleSystem` texts + the two fixed lines used in `claudesdk.systemPrompt`), `func historyItems(history []task.Message) []map[string]any`, `func dynamicTools(tools []task.ToolSpec) []map[string]any`.

- [ ] **Step 1: Failing table test** `TestHistoryItems`: user text → `{type:message, role:user, content:[{type:input_text,text}]}`; assistant text → `{type:message, role:assistant, content:[{type:output_text,text}]}`; assistant call `{ID:"c1",Name:"vmhealth.read",Args:{"host":"a"}}` → `{type:function_call, call_id:"c1", name:"vmhealth__read", arguments:"{\"host\":\"a\"}"}`; tool result → `{type:function_call_output, call_id:"c1", output:<content>}`; system messages skipped; a call with no later result gets `output:"error: no result recorded"`. `TestDynamicTools`: name mapped, `inputSchema` passed through, `type:"function"`.
- [ ] **Step 2:** run → FAIL. **Step 3:** implement. **Step 4:** PASS.
- [ ] **Step 5: Commit** `codex: map history and tools to app-server items`

### Task 2: `Adapter.Step` over app-server

**Files:**
- Modify: `adapters/model/codex/codex.go`
- Test: `adapters/model/codex/codex_test.go` (fake app-server via re-exec helper process, scripted by env `CODEX_FAKE=<script>`)

**Interfaces:**
- Produces: `type Config struct { Command, Home, Model string }`, `func New(c Config) *Adapter`, `func (a *Adapter) Step(ctx, history, tools) (task.AssistantTurn, error)` (satisfies `app.Model`), `var ErrBuiltinTool = errors.New("codex tried a built-in tool")`.

- [ ] **Step 1: Failing tests** (fake records every client message):
  - `TestStepText`: fake answers initialize/thread/start/inject/turn, emits `item/completed {item:{type:agentMessage,text:"ok"}}` then `turn/completed` → `Text=="ok"`, no calls; recorded `thread/start` params equal the fixed fields; `inject_items` sent before `turn/start`; `turn/start.input == []`.
  - `TestStepToolCall`: fake sends request `item/tool/call {callId:"x1", tool:"vmhealth__read", arguments:{host:"alpha"}}` → turn has one call `{ID:"x1", Name:"vmhealth.read"}`; client sent `turn/interrupt` and never a response to that request id.
  - `TestStepBuiltinTool`: fake sends `item/commandExecution/requestApproval` → `errors.Is(err, ErrBuiltinTool)`; client replied with a JSON-RPC error to that id.
  - `TestStepEnv`: env is exactly PATH, HOME, CODEX_HOME=<cfg.Home>.
  - `TestStepCancel`: cancel mid-turn → returns `context.Canceled` within 2 s.
- [ ] **Step 2:** run → FAIL.
- [ ] **Step 3: Implement**: one goroutine reads stdout lines into a channel; requests matched by id; notifications/requests handled in a select loop with `ctx.Done()`. `model` passed in `thread/start` only when set. Usage: read it from whichever notification the real binary sends (check with the spike before coding); if none, leave zero and add `// TODO: uncertain — app-server usage notification not confirmed, verify before prod`.
- [ ] **Step 4:** PASS with `-race`.
- [ ] **Step 5: Commit** `codex: one Step per app-server turn with dynamic tools`

### Task 3: Config, wiring and auth

**Files:**
- Modify: `adapters/config/config.go` (add `Codex struct { Command, Home string }` yaml `codex`), `cmd/haythosd/main.go` (`case "codex"`, `auth codex`), `config/haythos.yaml` (commented example `codex-sub: { kind: codex }`), `README.md`
- Test: `adapters/config/config_test.go`, `cmd/haythosd/main_test.go`

**Interfaces:**
- Consumes: `codex.New`, `codex.Config` from Task 2.

- [ ] **Step 1: Failing tests**: config `codex: {command: codex, home: data/codex}` loads, defaults applied when empty; `buildModels` with a `kind: codex` model and a missing `auth.json` in `Home` skips it with the same warning style as other kinds; `auth codex` runs `codex login --device-auth` with `CODEX_HOME=<home>` (fake runner, assert argv + env).
- [ ] **Step 2:** run → FAIL. **Step 3:** implement (create `Home` with 0700). **Step 4:** `make gate` green.
- [ ] **Step 5: Commit** `codex: wire kind codex and haythosd auth codex`

### Task 4: Live check (with the user)

- [ ] `haythosd auth codex`, then `run-job vm-health` on `codex-sub`: report arrives, ledger shows the tool ran through the gateway, `CODEX_HOME` holds no user memory/hooks. Record latency and any usage numbers in `docs/REPORT.md`.

---

**Decisions needed before Task 3:**
1. Codex keeps its own `auth.json` (with refresh token) in `CODEX_HOME`, outside the vault, because the binary refreshes it itself. Accept, or require it encrypted at rest some other way?
2. OpenAI has no explicit statement on third-party apps using ChatGPT-plan auth through the official binary (FINDINGS §6). Accept the gray area for a personal kernel?

**Not in this plan:** cheaper models via `kind: openai` + `base_url` (config only, already supported); `/model` switching across providers (selector is claudesdk-only today).
