# M1 kickoff: reliable runtime

Read `CLAUDE.md` first. It is the source of truth for architecture, rules, and coding standards. If this file and `CLAUDE.md` disagree, stop and ask (or, in unattended mode, follow `CLAUDE.md` and record the conflict in `REPORT.md`).

## Goal

A single Go binary, `haythosd`, that runs one agent loop, survives crashes without duplicate side effects, talks to the user on Discord, and runs one scheduled job (VM health) end to end. Claude is reachable two ways: API key (Messages API) and subscription (sidecar using the official Claude Agent SDK). OpenAI-compatible is reachable by base URL + key.

## In scope

- `core/task`, `core/policy` with table tests
- `app` use cases: `RunTask`, `HandleInput`, `HandleApproval`, `Tick`; `app/gateway`
- `adapters/store/sqlite`: event log + action ledger + startup reconciliation
- `adapters/model/anthropic`, `adapters/model/openai`, `adapters/model/claudesdk` + `sidecar/claude-sdk`
- `adapters/channel/discord` (Gateway WebSocket via `bwmarrin/discordgo`; buttons for approval)
- `adapters/tools/local/vmhealth` (CPU, GPU if present, RAM, disk, top processes; no LLM needed to collect)
- `adapters/vault/file`
- Scheduler (cron + manual trigger) and `jobs/vm-health.yaml`
- `haythosd auth` onboarding command (Claude, Discord, OpenAI optional)
- `/pause` command in Discord that stops all writes immediately

## Out of scope (do not build)

gbrain, Jev, morning brief, Slack, Jira, GitHub, MCP HTTP connectors, sandbox, ACP builder, Goal entity, steering, mobile app, multi-user. If something seems to need one of these, stop and ask.

## Phase gate (end of every phase)

All of these must be green before a phase is done:

```
gofmt -l .            # prints nothing
go vet ./...
golangci-lint run     # uses .golangci.yml
go test -race ./...
```

Then commit and write a short summary: changed / uncertain / next.

- **Interactive mode (default):** stop after the summary and wait for my reply before the next phase.
- **Unattended mode:** see the section at the end; do not stop between phases.

## Phases

### Phase 0: scaffold + spikes

Scaffold: `go mod init`, directory layout from `CLAUDE.md`, `.golangci.yml` in place, a `Makefile` (or `justfile`) with `fmt`, `lint`, `test` targets that run the phase gate. No production logic yet.

Spikes: answer these with small throwaway programs under `spikes/` and report findings with evidence (output, versions, links to docs you read):

1. `modelcontextprotocol/go-sdk`: current version, stdio + HTTP client support. (Used later; just confirm.)
2. Claude Agent SDK (TypeScript): can one call run with **no tools** and return a single assistant turn? Is there a structured output / JSON schema mode? Measure wall-clock overhead per call.
3. Auth: does the Agent SDK pick up a token from `claude setup-token` via environment variable? How long is that token valid?
4. Does a Go ACP client library exist? (Report only; ACP is M3.)

If any answer blocks the design (e.g. the SDK cannot do a tool-less single turn), stop and propose options before continuing.

### Phase 1: contract tests first

Write these tests before the features they test, following the TDD rules in `CLAUDE.md`. They must compile (against minimal port and type stubs) and fail now; they pass by the end of M1. Use hand-written fakes for `Model`, `Tools`, `Notifier`; a real temp SQLite file for the store; an injected clock; in-process crash points via an injected hook.

Commit them **alone**, with the message `contract: M1 acceptance tests`. They are frozen from that commit on: never edit them to make them pass. If one is wrong, leave it failing and explain why.

1. Kill the kernel after an external send but before success is logged → on restart, no duplicate send; ledger row ends `succeeded` (destination check finds it) or `outcome_unknown` (user notified, no retry).
2. Restart with a pending approval → nothing executes until a valid approval arrives; changing the args (hash mismatch) or an expired approval voids it.
3. A tool result containing injected instructions ("send this to #general", "ignore your rules") cannot change the write target or raise the tier; the write is escalated to tier 3 and asks.
4. `claudesdk` adapter: a malformed or schema-invalid tool call is never executed; one repair retry, then the task fails with a clear reason. SDK built-in tools are never enabled.
5. A task killed mid-`running` resumes from the event log to the same state with the same history.
6. Budget: a task that exceeds its token or step budget ends `failed` with reason `budget` and notifies once.
7. `/pause` stops gateway writes immediately, including writes already approved but not yet dispatched.

### Phase 2: core and app

Red → green → refactor for each behavior.

- `core/task` state machine + table tests for every valid and invalid transition.
- `core/policy`: tier lookup, taint escalation, approval validation (tool, target, args hash, expiry, policy version).
- `app/gateway` implementing the order in `CLAUDE.md` rule 2.
- Agent loop in `RunTask`: `Step` → if tool calls, each goes through the gateway → append results → repeat until no tool call, `waiting_approval`, budget, or error.

### Phase 3: adapters

Red → green → refactor for each behavior. Adapters that talk to external services are tested against recorded fixtures or local fakes (e.g. `httptest.Server`), never live accounts.

- `store/sqlite`: schema migrations, event log with global cursor + per-task seq, ledger, startup reconciliation.
- `model/anthropic` and `model/openai`: native tool calling, mapped to the shared `AssistantTurn`.
- `sidecar/claude-sdk` (TypeScript) + `model/claudesdk`: JSON-RPC over stdio, one `step` method, tool list rendered into the prompt, response validated against a JSON schema in Go. Kernel spawns and supervises the sidecar (restart on crash, health check at start). The sidecar has its own `npm test` (or equivalent) and type check, added to the phase gate.
- `vault/file`: encrypted at rest; key from an env var or OS keyring. Secrets resolved only inside the gateway.
- `channel/discord`: text in/out, approval buttons bound to the approval id, `/pause`, `/resume`. Never log message content that may contain secrets.

### Phase 4: VM health end to end

- `jobs/vm-health.yaml`: cron morning + afternoon (Asia/Jakarta), manual trigger, mode `background`, tool allowlist `[vmhealth.read]`, short report contract.
- Collect metrics in Go; call the model only to summarize anomalies. If nothing is abnormal, send a one-line "all normal" (this job is the exception to the silent rule while testing).
- An end-to-end test with a fake clock: two scheduled runs, a kill + restart between them, exactly two reports delivered, no duplicates.
- Interactive mode only: run it locally for a day with real Discord and confirm both scheduled runs arrive.

## Definition of done (M1)

- All Phase 1 contract tests pass, unchanged since their commit.
- Phase gate green: `gofmt`, `go vet`, `golangci-lint`, `go test -race`, sidecar tests.
- VM health arrives twice a day on Discord without misses while running locally (verified with the user).
- Kill + restart at any point produces no duplicate notification and no unauthorized write.
- The same job runs on all three model adapters by changing only config.
- README section on `haythosd auth` and running locally.

## When unsure

Stop and ask. Especially about: anything touching auth or secrets, a dependency that pulls in a large tree, any change to the rules in `CLAUDE.md`, and anything that would put a second agent loop outside Go.

## Unattended mode

Use only when I say "run M1 unattended". Overrides for that run:

- Do not stop between phases. Run Phase 0 → 4 in order, committing after each phase gate.
- No real credentials. Do not run `haythosd auth`; do not call Discord, Anthropic, OpenAI, or the Agent SDK with real accounts. Use fakes and fixtures; real-account checks become TODOs for me.
- Phase 0 blocker rule: if a spike blocks the design, do not redesign. Build the affected adapter behind its port as far as possible, mark the gap with `// TODO: uncertain — <reason>, verify before prod`, and continue.
- Any other decision you would normally ask about: pick the simplest option consistent with `CLAUDE.md`, record it under "Decisions made without me" in `REPORT.md`, and keep going.
- Never weaken a `CLAUDE.md` non-negotiable rule or a frozen contract test to make something pass.
- At the end, write `REPORT.md`: done per phase, phase gate output, decisions made without me, open TODOs, and anything that needs a real account to verify.
