# M1 report (unattended run)

Run date: 2026-10-27 (Asia/Jakarta). I treated "get things done tonight, I'll check in the morning" as "run M1 unattended" from `M1_KICKOFF.md`: no stops between phases, no real credentials, no live calls to Discord, Anthropic, OpenAI or the Agent SDK.

## Done per phase

| Phase | Commit(s) | Result |
| --- | --- | --- |
| 0 scaffold + spikes | `scaffold: ...` | Module, layout, `.golangci.yml`, Makefile, `experiments/FINDINGS.md`. Spikes 1 and 4 confirmed, 2 and 3 partly (static evidence only, see TODOs). |
| 1 contract tests | `stubs: ...`, `contract: M1 acceptance tests` | 7 contract tests committed alone, failing against stubs. `tests/acceptance/m1_test.go` has 0 diff lines since that commit. |
| 2 core + app | `core,app: ...` | `core/task`, `core/policy`, `app/gateway`, agent loop, SQLite store (needed so the contract tests could run). |
| 3 adapters | `adapters: ...` and later commits | sqlite, anthropic, openai, claudesdk + TS sidecar, vault/file, discord, local tools + vmhealth. |
| 4 VM health | later commits | `jobs/vm-health.yaml`, scheduler `Tick`, `run-job`, `auth`, e2e test with fake clock, README. |

All 7 Phase 1 contract tests pass. Honest gate history: the Phase 1 commits had `go test` red by design; the Phase 2 commit had one contract test (claudesdk) still red because that adapter was Phase 3 work; fmt, vet and lint were green at each commit. Only the final state below was gated as a whole.

## Phase gate (final, `make gate`, exit 0)

```
0 issues.
ok    github.com/panca/haythos/acceptance  (cached)
ok    github.com/panca/haythos/adapters/channel/discord  (cached)
ok    github.com/panca/haythos/adapters/model/anthropic  (cached)
ok    github.com/panca/haythos/adapters/model/claudesdk  (cached)
ok    github.com/panca/haythos/adapters/model/openai  (cached)
ok    github.com/panca/haythos/adapters/store/sqlite  (cached)
ok    github.com/panca/haythos/adapters/tools/local/vmhealth  (cached)
ok    github.com/panca/haythos/adapters/vault/file  (cached)
ok    github.com/panca/haythos/app/gateway  (cached)
ok    github.com/panca/haythos/cmd/haythosd  (cached)
ok    github.com/panca/haythos/core/policy  (cached)
ok    github.com/panca/haythos/core/task  (cached)
ok    github.com/panca/haythos/internal/config  (cached)
ℹ pass 4
ℹ fail 0
```

`go test -race -count=5 ./tests/acceptance/` passed (no flakes seen).

## Decisions made without me

1. **Module path** is `github.com/panca/haythos`, a placeholder. Change with `go mod edit -module` plus a find/replace of the import path.
2. **Git identity**: this repo has no local git config, so commits use your global identity (work email `<work email>`). It is a personal project; fix author before pushing. I left out the Claude co-author trailer on every commit, as your global CLAUDE.md requires (it overrides the harness default).
3. **`golangci.yml` renamed to `.golangci.yml`** (kickoff says that name). I excluded `contextcheck` for `_test.go` in it because the frozen tests use context-free helpers; I did not touch the tests.
4. **Schedule**: 07:00 and 15:00 Asia/Jakarta (kickoff says morning + afternoon; the spec says morning + evening).
5. **`HandleApproval(ctx, taskID, approvalID, approve)`** takes the task id as well, and approval ids are `ap-<callID>`. Frozen tests use this signature.
6. **Approvals live in the event log**, not in memory, so a pending approval survives restart. TTL is fixed at 1 hour (`Deps.ApprovalTTL`, no config key yet).
7. **Dispatching ledger rows after a crash**: destination check finds it, then `succeeded`; anything else (not found or cannot verify) becomes `outcome_unknown` with an alert and no retry. Conservative on purpose.
8. **VM health**: the kernel runs `vmhealth.read` itself (`Job.Collect`). If nothing is abnormal and nothing is unmeasurable, it sends `normal_msg` without calling the model; otherwise the model summarizes. Missing sources (e.g. no `/proc` on macOS) count as gaps, not "normal".
9. **Quiet rule and output-contract check are not built** (contract checks are M2 in `CLAUDE.md`). VM health always reports, as the kickoff allows while testing. `mode` and `contract` in job YAML are parsed and validated but unused.
10. **Tool names** with dots (`vmhealth.read`) are sent to Anthropic/OpenAI as `vmhealth__read` and mapped back, since those APIs reject dots.
11. **`/pause`** is stored in the event log (`_system` stream) so it survives restart. Pending and running tasks are resumed in `Start`.
12. **Vault key** comes only from `HAYTHOS_VAULT_KEY`. OS keyring is not done.
13. **Node sidecar** runs TypeScript with Node's built-in type stripping (`node src/index.ts`, Node 22.18+; this machine has v26). `tsc` is used for type checking only.
14. **Dependencies added** (all justified by the kickoff or the lack of a stdlib option): `modernc.org/sqlite` (pure Go, no cgo), `bwmarrin/discordgo` (named in the kickoff), `robfig/cron/v3` (cron parsing, used in `app/schedule.go`; this is the one external import in `app`), `gopkg.in/yaml.v3`, `golang.org/x/term` (hidden input for `auth`), `google/go-cmp` (allowed). **Flag**: discordgo pulls an old `golang.org/x/crypto` (2021 pseudo-version); check it before running on a VPS.
15. The MCP Go SDK is only spiked (`experiments/mcp`, its own module), not a dependency of the main module.

## Live checks (2026-10-28, with the user)

- `run-job vm-health` on `claude-sub` (subscription token from `haythosd auth claude`) worked end to end on macOS: sidecar → Agent SDK 0.3.310 → report on stdout. A tool-less single turn with `outputFormat: json_schema` and `maxTurns: 1` works live.
- The report quoted a fake "0% memory" because unmeasured metrics were sent as 0. Fixed: unmeasured metrics are left out of the report and only their gap is sent.
- `claude-sub` can now pick a model (`model:` on a `kind: claudesdk` entry, passed as SDK `options.model`); empty means the SDK default.
- Spike 3 (`experiments/agentsdk/token-precedence.mjs`): with an invalid `CLAUDE_CODE_OAUTH_TOKEN` the SDK fails with `401 OAuth access token is invalid`, with the real or an empty `HOME`. Without a token it fails with `Not logged in`. So the env token is what authenticates and the sidecar does not fall back to the Mac's Claude Code login.
- Spike 2 overhead (`experiments/agentsdk/probe.mjs`, same options as `step.ts`, 3 calls, Agent SDK 0.3.310, model = SDK default): 4.3 s, 4.7 s, 6.0 s per call for a one-word reply. Each call spawns a Claude Code subprocess and shows 2 assistant messages, yet still fits `maxTurns: 1`. Budget about 5 s per `Step` on `claude-sub`; a multi-step task pays this on every step. This run authenticated with the machine's existing Claude login, not the vault token (Spike 3 covers that path).

## Open TODOs (marked in code with `TODO: uncertain`)

- Notices are at-least-once between `Publish` and the `notice.sent` record: a crash in that window can repeat one notice (`app/app.go`, `notify`). A failed Discord publish is logged and not retried later, so a report can be missed if Discord is down at that moment.
- Vault: add OS keyring.
- Kill and restart is tested at four named crash points (before dispatch, after execute, after tool result, mid VM-health run), not "at any point".

## Needs a real account or you (not verified)

1. Spike 3: validity period of `claude setup-token` (sources say about 1 year, unofficial). Token precedence is confirmed (see Live checks).
2. Discord end to end: create the bot, enable Message Content intent, set `owner_id` and `channel_id`, check buttons and `/pause` live. Slash commands are registered globally and can take a while to appear.
3. The one-day local run with both scheduled reports arriving (Definition of done).
4. Same job on all three model adapters with real keys: `claude-sub` done; `claude-api` and `openai` still only fixtures and `httptest`.

## Nearby issues I noticed and left alone

- `core/policy/policy_test.go` has an unused `mutate` field in the approval table test (harmless clutter).
- The spec doc's `Model` port (`Complete`, `Stream`) differs from `CLAUDE.md` (`Step`); I followed `CLAUDE.md`.
