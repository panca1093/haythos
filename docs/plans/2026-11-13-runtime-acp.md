# runtime: delegate heavy work over ACP (plan, 2026-11-13)

## Why

Rule 1 says heavy work goes to a runtime through the `Runtime` port, and that port is still only a plan. The old plan was one adapter per runtime: Claude Code first, then Codex. That way every new runtime costs a new adapter.

Axel (`lgalabru/axel`, read 2026-11-13) drives 12+ harnesses through one Agent Client Protocol (ACP) layer: OpenCode, Goose, Cursor, Gemini and others speak ACP themselves, and Claude Code and Codex get an adapter. HaythOS takes the transport idea and leaves the rest of Axel out (signed multi-replica log, agent identities, Rex).

| Pattern | Taken from |
|---|---|
| One ACP client; a runtime is a command plus config | Axel `axel-acp/src/adapter.rs` |
| Claude Code driven directly over `stream-json` and its control protocol, no Node | Axel `axel-acp/src/claude/mod.rs`, our `claudesdk/cli.go` |
| Session identity in the runtime's env | Axel ADR 0003 (`AXEL_THREAD`, `AXEL_SESSION`) |
| A session's end is a submission; the contract decides `done` | Axel RFD 0002 |

What we leave out, and why:
- **Turn checkpoints / undo:** they matter once runtimes write unattended. They come after the first delegated task works.
- **Several sessions per task, agent teams, graphs:** one delegated session first.
- **Sandbox:** out of scope until the owner asks (rule 2).
- **`fs/*` and `terminal/*` client methods:** we don't advertise them, so runtimes use their native tools.

## Decisions

1. **Three adapters behind one port: `runtime/acp`, `runtime/claudecli`, `runtime/codexapp`.** *(Owner decision, 2026-11-13; `codexapp` added after the spike.)*
   - `acp`: a stdio JSON-RPC client. Each runtime that speaks ACP itself (OpenCode, Goose, …) is a config entry (command, args, config-option map, guard rules).
   - `claudecli`: a long-lived `claude` running `stream-json` in both directions. Using the official CLI keeps rule 5. No Node, since the sidecar was removed on 2026-11-09.
   - `codexapp`: native `codex app-server`, reusing the `acp` JSON-RPC client; only the method names and the event mapping differ. Not `codex-acp`: it is Node and wraps the same app-server, so it adds a layer and Node for nothing.
   - A runtime gets its own adapter only when it does not speak ACP and its ACP wrapper would need Node.
2. **Session events are ACP-shaped.** All runtime adapters emit the same `RuntimeEvent` kinds: `message`, `thought`, `tool_call`, `tool_call_update`, `plan`, `permission_request`, `mode`, `stop`. The event log, Discord and the TUI render one format.
3. **Ids are HaythOS's.** The kernel mints the session id (`HAYTHOS_SESSION`). The runtime's own session id is stored as a reference, for resume only.
4. **A permission request is decided by Alfred (decision 7).** When the policy says ask, `permission_request` moves the task `running → waiting_approval` and goes to the origin channel. The answer goes back through `Session.Approve`, which answers the pending request. It is never typed into the session.
5. **A delegated `done` needs the contract.** `stop: end_turn` is a submission. v1 contract: the stop reason is `end_turn` and the workdir has a change (a diff or a new commit). Otherwise the task is `failed`, with the reason recorded. Checks such as `go test` are decision 5b.
   **Decision 5b: checks are part of the contract.** *(Owner decision, 2026-11-13.)*
   - **Declared** in the job YAML as `checks:`, a list of `{name, argv, timeout}`. `argv` is a string list, never a shell string. Example: `argv: [go, test, ./...]`, `timeout: 10m`.
   - **Authored** by the job definition in `jobs/` or config only. Chat-created jobs (`JobDef`) get no `checks` field, and neither the model nor the runtime can set or change them. Rule 7: the workdir comes from config, not from the check.
   - **Run** by the kernel after `stop: end_turn`, once, in order, in the task's workdir, with env `PATH` and `HOME` only (plus `HAYTHOS_TASK`). Stdout and stderr are capped (about 8 KiB each, tail kept, since test failures print last). On timeout the process group is killed.
   - **Result:** exit 0 = pass. Anything else, a timeout or a start error = fail. One event per check: name, argv, exit code, duration, capped output. The output is data, not instructions: it taints the task like any tool output and is never fed back to the model as a command.
   - **On failure** the task goes `running → failed` with reason `check <name> failed (exit N)`. No automatic re-prompt in v1. Re-prompting the session is a second `Send` and a retry policy; it needs its own decision once a real failure shows it is worth it.
   - **Layering:** `app.RunDelegated` calls a `RunCheck func(ctx, dir, argv, env, limit)` field in `app.Deps`, like `vmhealth.Runner`: a function, not a new interface, because `app` may not import `os/exec` and a test fake is one closure. The real one is a small adapter, a variant of `readfile.Run` that does not inherit the full environment. `core/contract` stays pure (stop reason, diff present, check results to verdict).
   - **Why this is not "shell from the kernel's loop" (tier 4):** the kernel's model loop never sees or calls it, there is no shell, and the argv is fixed by the owner. It follows `collect:` (the kernel runs a fixed collector at task start) and `vmhealth` (fixed argv, no shell).
   - **Uncertain:** (1) a repo's own `go test` runs repo code with the user's permissions, so a runtime can plant code that the check then runs; the runtime already has the same permissions, so this adds no new power, but it is not isolation. (2) `go test` with `HOME` only may miss `GOPATH`/`GOFLAGS`/`GOPRIVATE`; verify on a real repo in phase 4.

6. **Chunks are coalesced before the event log.** `message` and `thought` chunks are buffered per message and appended once. Tool calls and permission requests are appended as they arrive. *(Claude's call: keeps the log readable and its size bounded.)*
7. **Alfred owns the guard, the runtime is only the stop point.** *(Owner decision, 2026-11-13.)* Each runtime is configured to ask for every shell command (and for reads of credential paths); file reads and edits in the workdir stay auto. Every `permission_request` goes through one pure function in `core/policy` with the guarded list from config: `allow` (answered at once), `ask` (`waiting_approval`, origin channel) or `deny`. Every verdict is an event. One policy in Go, table-tested, the same for every runtime, instead of a guard list translated into three runtimes' own rule formats.
   - Runtime config for this: claude `default` mode with `Read`/`Edit`/`Write` allowed and no `Bash` allow rule (so every Bash call is a `can_use_tool`); opencode `permission.bash: {"*":"ask"}`; codex `approvalPolicy: untrusted` (it asks for almost every command, FINDINGS §8).
   - Still pattern matching on commands, so still a guard rail (rule 2), but now in one place.
   - Cost: a local round trip per shell command. opencode ends the whole turn on a reject (`stopReason: cancelled`), so a `deny` there ends the session's turn; open question 5.

## Port (app/ports.go)

```go
type Runtime interface {
	Name() string
	Capabilities() RuntimeCaps // Resume, Images, Modes
	Start(ctx context.Context, req RuntimeStart) (RuntimeSession, error)
}

type RuntimeStart struct {
	TaskID, SessionID string
	Workdir           string
	Model, Mode       string
	ResumeRef         string // runtime's own session id, empty for a new session
}

type RuntimeSession interface {
	Ref() string // runtime's own session id
	Send(ctx context.Context, prompt string) (StopReason, error)
	Approve(ctx context.Context, requestID, optionID string) error
	Events() <-chan RuntimeEvent
	Stop(ctx context.Context) error
}
```

The names are a draft; phase 1 freezes them in a contract test.

## ACP mapping (runtime/acp)

| Port | ACP |
|---|---|
| `Capabilities` | `initialize` → `agentCapabilities` (`loadSession`, prompt capabilities) |
| `Start` | spawn, then `initialize` with client capabilities `fs:false` and `terminal:false`, then `session/new` or `session/load` |
| `Send` | `session/prompt`; returns `stopReason` |
| `Events` | `session/update` notifications |
| `Approve` | the response to `session/request_permission` |
| `Stop` | `session/cancel`, then kill the process group |

Child environment: `PATH`, `HOME`, `USER`, `HAYTHOS_TASK`, `HAYTHOS_SESSION`, plus the env the runtime's config entry names (`USER`: claude on macOS needs it to find its keychain login, FINDINGS §8). Nothing else is inherited, the same as `claudesdk`.

## claudecli mapping

| Port | Claude CLI |
|---|---|
| `Start` | `claude -p --input-format stream-json --output-format stream-json --verbose --permission-prompt-tool stdio [--resume ref] [--model m]`, then `initialize` control request |
| `Send` | a `user` message; the turn ends at `result` |
| `Events` | `assistant` / `stream_event` / tool use and tool result messages, mapped to the kinds in decision 2 |
| `Approve` | the response to the `can_use_tool` control request |
| `Stop` | `interrupt` control request, then kill |

Unlike the `claudesdk` model adapter, tools are on and setting sources are on, because this is the runtime path, not Alfred's loop.

## Phases

Each phase ends with `make gate` and `go test -race ./...` green.

0. **Spike (no production code).** Drive `opencode acp` and `claude` stream-json by hand from a Go test behind `//go:build integration`. Record in `docs/FINDINGS.md`: the frames seen, the stop reasons, and how each runtime asks for a guarded action. → verify: a FINDINGS section with a real transcript for each.
1. **Port, events, fake, guard policy.** Add the `Runtime` types to `app/ports.go` and a scripted `testfakes.Runtime`. Add the runtime guard verdict to `core/policy` (decision 7). Add `app.RunDelegated`: start → send → append events → `permission_request` → policy → allow / deny / `waiting_approval` → stop → contract. Commit the contract test alone. → verify: table tests for approve, reject, cancel, crash mid-turn (startup reconciliation marks the task `failed`), and contract fail.
2. **`runtime/acp`.** A JSON-RPC stdio client: replies routed by id, notifications by method, server requests answered later. Add config entries. Tests use a helper process (the test binary re-execs itself as a scripted ACP agent). → verify: the phase 1 contract test passes with the ACP adapter swapped in, and a `-tags integration` run against `opencode acp` passes.
3. **`runtime/claudecli`.** Long-lived process, control protocol, mapping to the same events. Shares the exec helper with `claudesdk` if the overlap is real. → verify: the same contract test, plus a `-tags integration` run against the real `claude`.
3b. **`runtime/codexapp`.** `codex app-server` over the `acp` JSON-RPC client, `item/*`/`turn/*` mapped to the same events, `requestApproval` answered with `accept`/`decline`, an isolated `CODEX_HOME` (the spike used the owner's real `~/.codex`). → verify: the same contract test, plus a `-tags integration` run against the real `codex`.
4. **Jobs use it.** Add an optional `runtime:` field to the job YAML (alias, model, mode, workdir from config, per rule 7). → verify: a manual job delegates a small change in a scratch repo, the approval for a guarded action shows on Discord and in the TUI, and the task ends `done` with the diff recorded.

## Open questions

1. **Guard translation for Claude.** *Answered by the spike (FINDINGS §8):* `default` mode plus `ask` rules via `--settings` is enough, no hook. Never `bypassPermissions` (it ignores `ask`). Credential reads through Bash (`cat .env`) need `Bash(*.env*)`-style rules; it stays pattern matching. opencode: `permission.bash` ask patterns in `OPENCODE_CONFIG_CONTENT`. codex: `prefix_rule(..., decision="prompt")` with `on-request` + `danger-full-access`.
2. **Checks in the contract.** *Decided (owner, 2026-11-13):* see decision 5b. No re-prompt on a failed check in v1.
3. **Codex path.** *Decided (owner, 2026-11-13):* native `codex app-server`, see decision 1 and phase 3b.
4. **ACP spec drift.** *Answered:* protocol v1; `session/set_model` does not exist and `session/set_mode` is being removed. Model and mode are config options from `session/new`, set with `session/set_config_option`. opencode ends a rejected permission with `stopReason: cancelled` (claude and codex keep the turn going), so `RunDelegated` must not read that as a user cancel.
5. **opencode and Alfred's deny.** opencode ends the turn with `cancelled` on any reject, so decision 7's `deny` stops an opencode session rather than letting the model try another way. Check in phase 2 whether replying `reject_once` with a message, or `allow` plus a failing result, behaves better; otherwise accept it for v1.
