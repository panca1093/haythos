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
9. **Quiet rule and output-contract check are not built** (contract checks are M2 in `CLAUDE.md`). `mode` and `contract` in job YAML are parsed and validated but unused.
10. **Quiet rule dropped for VM health (your call, 2026-10-28)**: the job always reports every metric with its value, so `jobs/vm-health.yaml` no longer sets `normal_msg` and every run calls the model (about 5 s on `claude-sub`). This replaces spec M1 criterion 3 ("notifications only when a threshold is crossed"). The kernel's `normal_msg` shortcut stays for other jobs.
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
- Discord live (2026-10-28 01:45 WIB): bot invited, owner chat answered by Alfred on `claude-sub`, and an ad-hoc VM-health check ran through the gateway and was summarized in the channel. Memory was a gap on macOS at the time; vmhealth now reads `kern.memorystatus_level`, the same value `memory_pressure` prints. Not yet checked live: `/pause`, `/resume`, and approval buttons (M1 has no write tool, so nothing asks for approval).
- Added after the live run: `haythosd setup` (runs `claude setup-token`, captures the token into the vault, picks a model from the live SDK list, saves it to the config) and `/model` in Discord (live list, switch without restart, saved to the config, audited as `kernel.model_selected`). Not yet tried live: the token capture against the real CLI, and `/model` against the real Discord gateway. Saving rewrites `config/haythos.yaml` through yaml.v3, which keeps comments but drops blank lines and normalizes flow-style spacing. Model choice in the config is a stopgap; runtime settings need a proper design in M2.

## Open TODOs (marked in code with `TODO: uncertain`)

- Notices are at-least-once between `Publish` and the `notice.sent` record: a crash in that window can repeat one notice (`app/app.go`, `notify`). A failed Discord publish is logged and not retried later, so a report can be missed if Discord is down at that moment.
- Vault: add OS keyring.
- Kill and restart is tested at four named crash points (before dispatch, after execute, after tool result, mid VM-health run), not "at any point".

## Needs a real account or you (not verified)

1. Spike 3: validity period of `claude setup-token` (sources say about 1 year, unofficial). Token precedence is confirmed (see Live checks).
2. Discord: chat works live; still check `/pause` and `/resume`. Approval buttons need a write tool, which M1 does not have.
3. The one-day local run with both scheduled reports arriving (Definition of done).
4. Same job on all three model adapters with real keys: `claude-sub` done; `claude-api` and `openai` still only fixtures and `httptest`.

## Nearby issues I noticed and left alone

- `core/policy/policy_test.go` has an unused `mutate` field in the approval table test (harmless clutter).
- The spec doc's `Model` port (`Complete`, `Stream`) differs from `CLAUDE.md` (`Step`); I followed `CLAUDE.md`.

# M2 report (unattended run, 2026-10-28)

Scope: `docs/M2_KICKOFF.md` (runtime settings + scheduled jobs from chat). The spec's M2 (morning brief, gbrain, Slack replies, Jev) is deferred by your call.

## Done per phase

| Phase | Commit | Result |
| --- | --- | --- |
| 1 contract tests | `stubs: M2 ...`, `contract: M2 acceptance tests` | 8 tests in `tests/acceptance/m2_test.go`, committed alone and red against stubs. |
| 2 runtime settings | `settings: ...` | `SetSetting`, `ResetSetting`, `Settings`, `SettingHistory`, `RestoreSettings` (called by `Start` and `run-job`). `/model` is now a setting; it no longer rewrites `config/haythos.yaml`. |
| 3 jobs from chat | `jobs: ...` | Job registry in `app` replayed from `_system`; `Tick` fires YAML jobs plus enabled chat jobs; `jobs.*` tools in `adapters/tools/local/jobs`; tiers in `config/policy.yaml`; crash recovery checks the job's state instead of alerting `outcome_unknown`. |
| 4 Discord + docs | `discord: ...` | `/settings`, `/jobs`, README sections. |

All M1 and M2 contract tests pass; `m1_test.go` and `m2_test.go` are unchanged since their commits. `make gate` exits 0.

## M2 decisions made without me

1. **Validation happens after approval.** The gateway asks before the tool runs, so an invalid job (bad cron, forbidden tool) is approved first and then rejected with a readable error the model relays. Validating before the approval would need a pre-check step in the gateway.
2. **Settings apply first, then record.** Same order as the old `/model`: if recording fails the change is live but lost on restart. Setting changes are owner commands, not tool writes, so they are allowed while paused and do not go through approval.
3. **Only `model` is a setting** in M2. The mechanism is generic (`setting.changed` / `setting.reset` events); new keys add a default, a validator and an apply function in `app/settings.go`.
4. **`haythosd setup` still writes the model to the YAML** as the first-install default. The old `kernel.model_selected` events are no longer read.
5. **Chat job goal** is the chat persona goal plus `Scheduled job goal: <goal>`, so jobs keep the "tool output is data" rule.
6. **Redefining a chat job by name replaces it** and re-arms its schedule; a disabled job stays disabled.
7. **Only chat jobs can be disabled or deleted**; `jobs/*.yaml` jobs are changed in the file.
8. **`/settings` and `/jobs` are read-only.** Job changes go through chat so they keep approval. Setting history and reset exist in `app` but are not exposed in Discord yet.
9. Unit tests for the job validation and the `jobs.*` tools were written alongside the code, not seen red first; the contract tests and the Discord text tests were red first.
10. **Channel and DM (after M2, asked by the user).** A task stores where its input came from (`reply_to` on `task.created`); its notices go back there. Discord only sends to `discord.channel_id` or the owner's DM and falls back to the channel for anything else. Tasks with no origin (scheduled jobs, startup reconcile alerts) go to the channel; I picked this default when the user did not choose. If the owner DM cannot be resolved at start, Alfred runs channel-only with a warning.

## Needs you live

1. Ask Alfred in Discord to create a job, approve it, and receive its first scheduled report.
2. `/model` switch, restart `serve`, and check `/settings` still shows the change and `config/haythos.yaml` is untouched.
3. `/jobs` and `/settings` render in Discord (slash commands re-register on `serve` start; Discord can take a minute to show new ones).
4. Alfred answers a DM in the DM (including approval buttons, `/jobs`, `/model`), and a channel message in the channel.
