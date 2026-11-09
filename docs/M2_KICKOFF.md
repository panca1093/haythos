# M2 kickoff: runtime settings + scheduled jobs from chat

Read `CLAUDE.md` first. It is the source of truth for architecture, rules, and coding standards. If this file and `CLAUDE.md` disagree, follow `CLAUDE.md` and record the conflict in `docs/REPORT.md`.

M2 was re-scoped by the user on 2026-10-28. The spec's M2 (morning brief, gbrain, Slack replies, Jev) moves later. This M2 builds the platform those jobs will run on.

## Goal

1. **Runtime settings that can change any time.** `config/haythos.yaml` holds the defaults. Changes made at runtime are events in the `_system` stream, with history and reset. The effective value is the YAML default overridden by the latest change. `/model` moves onto this and stops rewriting the YAML.
2. **Scheduled jobs created from chat.** The user asks Alfred in Discord ("every Monday 09:00 check the disk and report") and a job with a cron, goal, and tools is created without writing YAML or restarting. Jobs can be listed, changed, disabled, enabled, and deleted the same way.

## In scope

- `app` settings: `SetSetting`, `ResetSetting`, `Settings` (effective values with their source), `SettingHistory`. A small registry of known keys, each with a validator and an apply hook. M2 keys: `model`.
- Startup replays setting overrides before the scheduler runs (for `model`: the claudesdk adapter gets the override).
- `app` job registry: chat-created jobs stored as events in `_system` (`job.defined`, `job.disabled`, `job.enabled`, `job.deleted`), replayed into the list `Tick` uses alongside the YAML jobs.
- Local tools, all through the gateway: `jobs.list` (tier 0), `jobs.define`, `jobs.disable`, `jobs.enable`, `jobs.delete` (tier 1, so they ask for approval in M2). The chat job's allowlist gets these tools.
- Validation of a chat-defined job: name is a lowercase slug and does not clash with a YAML job; cron parses (no `CRON_TZ` means `Asia/Jakarta` is prefixed); every tool exists and has tier 0 or 1 (`background` mode); the `jobs.*` tools are not allowed inside a job (no job creates jobs); budget has defaults and caps.
- Discord: `/settings` (effective values and source), `/jobs` (list with next run). Changes go through chat so they use approval.
- README: how to create a job from chat, `/settings`, `/jobs`.

## Out of scope (do not build)

gbrain, Jev, morning brief, Slack, Jira, GitHub, Calendar, MCP HTTP connectors, quiet rule, output-contract checker, sandbox, ACP builder, Goal entity, steering, settings keys other than `model`. If something seems to need one of these, record it and continue with the simplest option.

## Phase gate (end of every phase)

`make gate` exits 0 (gofmt, vet, golangci-lint, `go test -race ./...`, sidecar tests). Commit, then add a short summary to `docs/REPORT.md` under an M2 section: changed / uncertain / next.

## Phases

### Phase 1: contract tests first

Write these in `tests/acceptance/` (historically `m2_test.go`; now the file for the use case, e.g. `settings_test.go`, `jobs_test.go`) against minimal stubs. They must compile and fail now. Commit them **alone** as `contract: M2 acceptance tests`. Frozen from that commit on, like the M1 contract tests.

1. A setting change survives a restart; the latest change wins; reset returns to the YAML default; history lists every change in order.
2. An unknown key or an invalid value is rejected and writes no event.
3. Selecting a model through settings does not touch the config file, and a new app instance on the same database applies the override at start.
4. A job defined from chat asks for approval first; before approval it is not in the job list and never fires; after approval it fires at its cron time.
5. A chat-defined job survives a restart and fires once per due time (no duplicate across a kill + restart between `job.fired` and the run).
6. A job definition with a tool outside the registry, a tool above tier 1, a `jobs.*` tool, an invalid cron, or a YAML job's name is rejected with a tool error the model can read; no job event is written.
7. A disabled or deleted job does not fire. Re-enabling it does not fire the runs it missed while disabled.
8. A tainted task that tries to define a job is escalated to tier 3.

### Phase 2: runtime settings

Red → green → refactor. Settings registry and replay in `app`; `/model` (`app.SelectModel`) becomes a setting change; `config.SaveModelChoice` and the YAML rewrite are removed; `cmd/haythosd` applies overrides at start; `haythosd setup` keeps writing the YAML default (first install).

### Phase 3: scheduled jobs from chat

Red → green → refactor. Job registry in `app`, `jobs.*` tools under `adapters/tools/local/jobs`, `Tick` reads YAML + registry jobs, policy tiers in `config/policy.yaml`, `Exists` for the new write tools so a crash between dispatch and success does not define a job twice.

### Phase 4: Discord + docs

`/settings` and `/jobs` in Discord; README; `docs/REPORT.md` M2 section with what needs the user live (creating a job from Discord, approving it, receiving its first run).

## Definition of done (M2)

- All M1 and M2 contract tests pass, unchanged since their commits.
- Phase gate green.
- The user creates a job from Discord, approves it, and receives its scheduled report (verified live with the user).
- Switching the model with `/model` survives a restart and leaves `config/haythos.yaml` untouched.

## Unattended mode

The user asked for M2 unattended. Same overrides as M1:

- Do not stop between phases. Commit after each phase gate.
- No real credentials and no live calls to Discord or model providers. Live checks become items for the user in `docs/REPORT.md`.
- Any decision you would normally ask about: pick the simplest option consistent with `CLAUDE.md`, record it under "M2 decisions made without me" in `docs/REPORT.md`, and keep going.
- Never weaken a `CLAUDE.md` non-negotiable rule or a frozen contract test.
- Never commit the user's local edits to `config/haythos.yaml`.
