# Remove the job system (plan, 2026-11-26)

## Why

The owner will rebuild scheduling and background work in the Hermes/OpenBot or Claude Code style. The current job system goes first, including the `Job` struct (owner decision, 2026-11-26). Brain consolidate (02:00) and digest (07:00) stay, as kernel-internal schedules.

## What `Job` is today

`app.Job` is two things at once:

1. **The user-facing job product.** It covers:
   - YAML files in `jobs/`, loaded by `config.LoadJobs`.
   - `Trigger` (cron or manual), `DeliverTo`, `Checks`.
   - Chat-created jobs (`JobDef`, the `jobs.*` tools, `job.defined/enabled/disabled/deleted` events).
   - The cron path in `Tick`, `RunJob`, the `haythos run-job` CLI, the ws `job.run` frame, and Discord's jobs listing.
2. **The spec of every task.** It holds the name, goal, model, tools, budget, silent, brain source and runtime. Three things build one: chat (`DefaultJob`), brain extract, and consolidate/digest. `createTask` copies it into `task.Created`.

## Decisions (approved by the owner, 2026-11-26)

1. **Delete the job product completely.** This covers:
   - `jobs/` (3 YAML files) and `config.LoadJobs`/`jobFile`/`delegatedJob`.
   - `Trigger`, `JobDef`, `JobStatus`, `app/jobs.go`, `RunJob`, the user-job loop in `Tick`, and `launchDelegated`.
   - `adapters/tools/local/jobs`, `cmd/haythos/runjob.go`, the `-jobs` flag, the ws `job.run` frame, Discord's jobs list, and the `jobs.*` names in the default tool list.
2. **The task spec stays, unexported.** `Job` becomes `app.Spec`, with only the fields tasks still use: `Name`, `Goal`, `Model`, `Tools`, `Budget`, `Silent`, `BrainSource`.
   - `Deps.DefaultJob` becomes `Deps.Chat Spec`.
   - The runtime fields move into `Delegation`, which already has them: `RunDelegated(ctx, d Delegation)` drops its `Job` argument.
   - It is exported only because `cmd` and the test rigs build it. Removing it outright would mean six loose parameters on `createTask`, plus the same set on `Deps`.
3. **Brain schedules become kernel constants.** `Tick` keeps a fixed list of two internal schedules, at the same cron and under the same names. The existing `job.armed/fired` marks then still apply, and nothing fires twice after the upgrade.
   - `adapters/config` stops reading `jobs/`. The brain YAML goal moves into `brainmem.go`, next to `extractGoal`.
   - `robfig/cron` stays (the brain still needs it).
4. **Delegation loses its only entry point.** Today a runtime session starts only from a job (`RunJob` or cron).
   - After this change `RunDelegated` has no caller outside tests, until the rebuild adds one.
   - `CancelDelegated`, `Interrupt` on a delegated task, workspace checks and the runtime contract suite stay, tested through `RunDelegated` directly.

## Owner answers (2026-11-26): delete the job-only tests, leave delegation without an entry point, no migration

1. **Frozen acceptance tests.** Deleting jobs means deleting or rewriting frozen contract tests in `tests/acceptance`:
   - All of `jobs_test.go`: chat jobs, restart, invalid definition, disable/delete.
   - All of `delegate_job_test.go`: manual and cron delegated jobs, checks, guard rules.
   - Edits to `harness_test.go` and the rigs in `delegate_test.go`/`brain_test.go` that build `app.Job`.
   - Proposal: delete the job-only tests, and mechanically switch `app.Job{…}` to `app.Spec{…}` in the rest, with the same assertions. The checks and guard coverage from `delegate_job_test.go` is mostly duplicated in `delegate_test.go` (`TestDelegatedChecks`, `TestDelegatedGuards`); whatever is not gets noted in the PR.
2. **Delegation unreachable (decision 4).** This is fine until the rebuild, or a thin entry point stays (for example, keep the ws `delegate` path or a tool).
3. **Old events in the database.** `job.defined` events of chat jobs stay in the log, unread. Defined chat jobs (if any) stop firing silently. No migration is planned.

## Steps

1. Delete the job product (decision 1). **Verify:** `go build ./...`.
2. `Job` → `Spec`, plus `RunDelegated(ctx, Delegation)` (decision 2). **Verify:** `go vet ./...`.
3. Brain schedules as constants (decision 3). **Verify:** the existing brain tests pass, and an upgrade test where `job.armed` for `brain-consolidate` already exists does not re-arm or double-fire.
4. Tests: delete the job tests, rename in the rest (the open question above). **Verify:** `go test -race ./...` green.
5. Update CLAUDE.md: Jobs/skills/triggers section, the `jobs/` dir, and the `Runtime` row. **Verify:** no `jobs/` references left (`grep -rn "jobs/\|RunJob\|app.Job" --include=*.go`).
