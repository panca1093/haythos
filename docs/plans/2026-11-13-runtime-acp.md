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

1. **Two adapters behind one port: `runtime/acp` and `runtime/claudecli`.** *(Owner decision, 2026-11-13.)*
   - `acp`: a stdio JSON-RPC client. Each runtime is a config entry (command, args, mode map, guard rules).
   - `claudecli`: a long-lived `claude` running `stream-json` in both directions. Using the official CLI keeps rule 5. No Node, since the sidecar was removed on 2026-11-09.
   - Two implementations, so the port is a real interface.
2. **Session events are ACP-shaped.** Both adapters emit the same `RuntimeEvent` kinds: `message`, `thought`, `tool_call`, `tool_call_update`, `plan`, `permission_request`, `mode`, `stop`. The event log, Discord and the TUI render one format.
3. **Ids are HaythOS's.** The kernel mints the session id (`HAYTHOS_SESSION`). The runtime's own session id is stored as a reference, for resume only.
4. **A permission request is a task approval.** `permission_request` moves the task `running → waiting_approval` and goes to the origin channel. The answer goes back through `Session.Approve`, which answers the pending request. It is never typed into the session.
5. **A delegated `done` needs the contract.** `stop: end_turn` is a submission. v1 contract: the stop reason is `end_turn` and the workdir has a change (a diff or a new commit). Otherwise the task is `failed`, with the reason recorded. Running checks such as `go test` is open question 2.
6. **Chunks are coalesced before the event log.** `message` and `thought` chunks are buffered per message and appended once. Tool calls and permission requests are appended as they arrive. *(Claude's call: keeps the log readable and its size bounded.)*

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

Child environment: `PATH`, `HOME`, `HAYTHOS_TASK`, `HAYTHOS_SESSION`, plus the env the runtime's config entry names. Nothing else is inherited, the same as `claudesdk`.

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
1. **Port, events, fake.** Add the `Runtime` types to `app/ports.go` and a scripted `testfakes.Runtime`. Add `app.RunDelegated`: start → send → append events → `permission_request` ⇄ `waiting_approval` → stop → contract. Commit the contract test alone. → verify: table tests for approve, reject, cancel, crash mid-turn (startup reconciliation marks the task `failed`), and contract fail.
2. **`runtime/acp`.** A JSON-RPC stdio client: replies routed by id, notifications by method, server requests answered later. Add config entries. Tests use a helper process (the test binary re-execs itself as a scripted ACP agent). → verify: the phase 1 contract test passes with the ACP adapter swapped in, and a `-tags integration` run against `opencode acp` passes.
3. **`runtime/claudecli`.** Long-lived process, control protocol, mapping to the same events. Shares the exec helper with `claudesdk` if the overlap is real. → verify: the same contract test, plus a `-tags integration` run against the real `claude`.
4. **Jobs use it.** Add an optional `runtime:` field to the job YAML (alias, model, mode, workdir from config, per rule 7). → verify: a manual job delegates a small change in a scratch repo, the approval for a guarded action shows on Discord and in the TUI, and the task ends `done` with the diff recorded.

## Open questions

1. **Guard translation for Claude.** Can "full auto, but ask for the guarded list" be expressed with permission mode plus `ask` rules, or does it need a PreToolUse hook that returns `ask`? Not verified. Phase 0 answers it before phase 3.
2. **Checks in the contract.** Running `go test` after a session means the kernel executes a command. A fixed argv from the job definition is like `vmhealth`, not shell from the model's loop, but it needs an owner decision before it goes into decision 5.
3. **Codex path.** Use `codex-acp` (Zed's adapter, through `runtime/acp`) or drive `codex app-server` natively. Start with `codex-acp`. Not verified: how mature the adapter is, and whether it needs Node.
4. **ACP spec drift.** `session/set_model` and mode handling may still be unstable. Pin the protocol version in `initialize` and check the current spec in phase 0.
