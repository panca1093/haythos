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
2. **Git identity**: fixed on 2026-11-10; the repo uses a GitHub noreply address. I left out the Claude co-author trailer on every commit, as your global CLAUDE.md requires (it overrides the harness default).
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
- 2026-11-10: `claude-sub` now runs the `claude` CLI directly (Node sidecar removed). `run-job vm-health` with the vault token and scrubbed env returned a full report; 8.3 s wall clock including `go run` compile. CLI 2.1.319.
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
10. **Channel and DM (after M2, asked by the user; delivery superseded by items 11-22).** A task stores where its input came from (`reply_to` on `task.created`); its notices go back there. Discord only sends to `discord.channel_id` or the owner's DM and falls back to the channel for anything else. Tasks with no origin (scheduled jobs, startup reconcile alerts) go to the channel; I picked this default when the user did not choose. If the owner DM cannot be resolved at start, Alfred runs channel-only with a warning.
11. **`discord.channel_id` is still decoded but ignored** (`internal/config`, with a startup warning). Your local `config/haythos.yaml` still has the key and I may not edit that file; removing the field outright would make strict config loading, and so `TestShippedConfigLoads`, fail until you delete the line. Delete it from the yaml, then the field can go.
12. **Trigger rules are one pure function** (`decide` in the Discord adapter). Mentioned = the bot is in the message's mention list, so a reply that pings the bot counts. A message that is only the mention (nothing left after stripping it) is ignored.
13. **Thread state is two events in `_system`**: `thread.activated` and `thread.mention_only` (latest wins). A thread can have the flag without being activated; then it still needs a mention to join, and `/mention-only` says so.
14. **Thread is made public, auto-archive 24 hours**, named from the first 100 characters of your message. Sending in an archived thread unarchives it. If the thread cannot be created, Alfred posts the error in the channel and does not run the task.
15. **Intent `Guilds` added** so the bot's state knows threads; channel type checks fall back to the REST API when state misses.
16. **`/mention-only` outside a thread** replies ephemerally that it only works in threads. Slash commands and approval buttons now work anywhere for the owner (before: only in the configured channel or the DM).
17. **A task stores its targets in `task.created` (`targets`)**; `reply_to` stays and old events replay unchanged. `notice.sent` gained a `target`; old rows (no target) still dedupe tasks without targets. One failed target does not block the others; the next sweep retries only the failed one (approvals; a failed final report is not retried, as before).
18. **Default delivery when `deliver_to` is empty** is the defining task's origin (`ReplyTo`), carried to `DefineJob` on the context inside `app` (not visible to the gateway or tools). A DM origin is stored as the DM channel ID, which the adapter treats like `dm`. Redefining a job with empty `deliver_to` takes the redefining task's origin. The approval preview shows the tool arguments only, so an origin default is not visible when you approve.
19. **`deliver_to` entries are trimmed and de-duplicated**; their meaning is not checked in `app`. The adapter validates at send time: `""`, `dm` or the DM channel ID go to the DM; a text, news or thread channel the API returns with a guild goes there; anything else goes to the DM with a note, and if the DM is unknown the notice fails. The old `channel_id` fallback is gone, and `Open` now fails if the owner DM cannot be created.
20. **`discord.channels` is tier 0 and marked untrusted**: channel names are text other people can set, so using it taints the task like other external content. A consequence: a `jobs.define` after it may show the "escalated" note when the job name was not in your message (still always asks).
21. **No mentions fire from Alfred's messages** (`AllowedMentions` empty on every send), because reports can now land in shared channels and contain model-written or tool text such as `@everyone`.
22. **The Discord channel adapter exposes the tool as `ChannelsTool()`** and `cmd` registers it only when Discord is configured; this keeps adapters from importing each other. It lists at most 200 guilds and only active threads.
23. Pure rules, `app` methods and the `jobs.define` schema were red first. The Discord API calls themselves (`MessageThreadStartComplex`, `Channel`, `UserGuilds`, `GuildChannels`, `GuildThreadsActive`) are not covered by tests.
24. **`discord.post` is tier 2, chat only, added to the chat job only when Discord is configured** (`discordChatTools` in `cmd/haythosd`). Background jobs already reject it in `validJob` (tier above 1); a test pins that. Every post asks for approval because the allowlist is empty; taint escalation to tier 3 is the gateway's.
25. **Allowed targets**: `dm` (or the owner DM ID) or a guild text, news or thread channel the API returns with a guild. Anything else, including other people's DMs and unknown IDs, returns a tool error and sends nothing. Unlike delivery there is no fallback to the DM, because a post that silently lands elsewhere is wrong.
26. **No idempotency nonce.** The installed discordgo (v0.29.0) has no `Nonce` or `EnforceNonce` on `MessageSend`, and the gateway's idempotency key never reaches a tool handler. I did not add a dependency or call the REST API by hand. `discord.post` has no existence check, so a crash between dispatch and success ends as `outcome_unknown` (`ErrCannotVerify`), is reported to you and is never retried. Scanning recent bot messages by content was rejected: it cannot tell a retry from an identical legitimate repost. For multi-chunk posts this means nothing is verified; if a later chunk fails, earlier chunks stay posted and the call is reported as failed, so a retry by the model would repeat them (it needs a new approval).
27. **Chunking is the existing byte split at 1900** (`chunks`), which can cut a multi-byte character at a boundary. Not changed here.
28. Argument validation, target validation, chunking, empty mentions and the chat-job rejection were red first (the rejection test passes without new code, since the guard already existed). The real `ChannelMessageSendComplex` call is not covered by a test.

## Needs you live

1. Ask Alfred in Discord to create a job, approve it, and receive its first scheduled report.
2. `/model` switch, restart `serve`, and check `/settings` still shows the change and `config/haythos.yaml` is untouched.
3. `/jobs` and `/settings` render in Discord (slash commands re-register on `serve` start; Discord can take a minute to show new ones).
4. Alfred answers a DM in the DM (including approval buttons, `/jobs`, `/model`), and a channel message in the channel. Verified by the user on 2026-10-28 (DM replies).
5. Delete `channel_id` from `config/haythos.yaml`, restart `serve`, and check the warning is gone and slash commands still answer (`/mention-only` appears after a minute).
6. In a server channel: a message without a mention is ignored; one with @Alfred opens a thread and answers there; a follow-up in the thread without a mention is answered. Check the bot has Create Public Threads and Send Messages in Threads.
7. `/mention-only` in the thread: reply is private; a message without a mention is then ignored, with a mention is answered. Restart `serve` and check the thread keeps both behaviours.
8. Ask in a thread for a job "report in #infra" (and one with no place named, then one in a DM): approve, check `/jobs` shows the targets, and the first report goes to each. Try a deleted or private channel ID and check the DM fallback note.
9. Ask Alfred to list channels (`discord.channels`) and check guilds with many channels or private threads look right (private channels the bot can see are listed).
10. Ask Alfred to post a short message in a server channel and in your DM: the approval shows the channel and text, the message appears once after Approve, and Reject sends nothing.
11. Ask him to post `@everyone hello` in a channel: it must appear without pinging anyone. Ask for an unknown channel ID and for a voice channel: refused with a readable error.
12. Post a text over 1900 characters and check it arrives as several ordered messages.

## M2B report

### M2B decisions (Phase 1: conversation memory)

1. **Turns are recorded on a per-conversation stream `conv:<reply_to>`**, not scanned from all tasks. `finish` appends a `conv.turn` event (task id, user text, final answer, time from the injected clock) before the task turns `done`, so a crash cannot lose it; readers drop duplicate task ids. Chosen over `_system` because lookup is one `Load` and `_system` is replayed whole at every start. Consequence: tasks finished before this change have no history, and failed tasks leave no turn.
2. **DM flag lives on `app.Input.DM`**, set from `facts.DM` in the Discord adapter. It is not stored on `task.created`: the 24 h rule is applied to the current input and each turn carries its own timestamp. A DM turn exactly 24 h old is still kept.
3. **History is injected as context messages when the task is created** (system summary, then user/assistant pairs), stored in the task's own events so a resumed task replays the same prompt. `task.Message` gained `Context bool`; `Replay` does not treat context messages as trusted text (taint escalation must not trust past answers) and does not count them as steps (budget).
4. **Summary is a `conv.summary` event on the same stream** holding the text and the task ids it covers. It is rolled: the previous summary text plus the overflowing turns go back into one `Step` (no tools, system instruction "Summarize ... at most 200 words") on the default job's model. Only turns not already covered count toward the 7,000-token window, so the same turns are never summarized twice, including after restart.
5. **Window is strict**: newest turns while the running total (`(len(user)+len(answer))/4`) stays at or under 7,000. A single turn above 7,000 tokens is folded straight into the summary.
6. **DM expiry invalidates a summary** whose covered turns are no longer all inside 24 h (the summary cannot be split). The remaining in-window turns are then windowed afresh.
7. **If the summary call fails** (error, timeout, empty text), a warning is logged (no bodies), nothing is stored, and the folded turns are left out of that one prompt; the next message retries. The user's message is never blocked by it.
8. **Summary calls are not metered** against any task budget (no task exists yet) and use `StepTimeout`.
9. A per-conversation lock (`app.lock("conv:<reply_to>")`) serialises history building so two quick messages cannot both summarize.
10. **Process deviation**: `Input.DM` was committed alone first (`2e0fdc1`) so the frozen contract commit compiles; the contract file is the only content of its own commit.
11. **Unit tests were written after the implementation** (not seen red); the five contract tests were red first.

### Changed

- `app/conversation.go` (new), `app/conversation_test.go` (new), `app/app.go` (`HandleInput` loads history, `createTask` takes optional prior messages, `finish` records the turn), `app/ports.go` (`Input.DM`), `core/task/task.go` (`Message.Context`), `internal/testfakes/fakes.go` (`Model.Histories`), `adapters/channel/discord/discord.go` (sets `DM`), `tests/acceptance/m2b_test.go` (new, frozen).

### Uncertain

- **Contract test M2B-5 is wrong and left failing, per the frozen-test rule.** It asserts `len(h) == 2` on `r.model.last()`, but `last()` is the history of the final step, which already contains the scripted `vm.read` call and its result, so it can never be 2. Everything else in that test passes. The one-line fix (`len(h) < 2 || ... || h[1].Context`) was checked locally (all five pass) and reverted. It needs your approval to apply.
- Summary quality and the 200-word instruction are unverified against a real model.
- Token estimate bytes/4 over-counts non-ASCII-light text and under-counts CJK/emoji; fine for the 7,000 budget here but not a real tokenizer.
- Conversation turns hold full answers in the event log (already the case for task messages); no retention policy.
- Answers to a thread that included untrusted tool content are replayed as plain context; they are not trusted as write targets but the model can still read them.

### Next

- Phase 2: MCP client + gbrain (see `docs/M2B_GBRAIN.md`).

### Needs you live

1. In a thread: ask something, then a follow-up that depends on it ("and the second one?"); Alfred should answer in context.
2. Same in a DM; a DM message a day later should not see yesterday's turns, a thread message should.
3. Paste several long answers into one thread until it exceeds roughly 28 KB of text, ask again, and check the reply still knows the early topic (summary) and that `sqlite3 <db> "select type from events where task_id like 'conv:%'"` shows one `conv.summary`.

### Phase 2 and 3 (parallel branches, merged 51d7a8d, fix 977d566)

**Changed**
- MCP stdio client (stdlib), tools `<server>.<tool>`, untrusted unless `trusted: true`; the child gets only PATH and HOME.
- gbrain `Brain` adapter pinned at `613400c`, `brain.recall` (0), `brain.remember` (1), `brain.forget` (1), `scripts/brain-setup.sh` (not run).
- Brain first: one `<recalled_memory>` block (≤3 hits, 3,200 bytes) before the first step of an owner chat task, recorded as `brain.recalled`.
- `auto_when_clean: [brain.remember, brain.forget]`: no approval when the task is untainted and its source is `owner/` or `auto/`; `auto/` forget only for `inbox/` ids.
- Zero-width/bidi characters stripped from recall and untrusted tool output.
- Stale hook (3 h), `brain-consolidate` (02:00 WIB, silent), `brain-digest` (07:00 WIB, Undo per item), suppress-embeds on tainted notices; state on the `_brain` stream.

**Decisions at merge**
- Brain-first recall runs only for `owner/` chat tasks. Extraction and consolidation recall with the tool; injecting recall there made extraction re-save an existing item.
- gbrain assigns its own fact ids and ignores a requested slug. The adapter returns unverified (auto) facts as `inbox/<id>` and strips the prefix on forget; the kernel tracks inbox saves from the tool result, not the request args.
- The two fake Brains were unified into one `testfakes.Brain` that satisfies both frozen test files.
- Deviation from CLAUDE.md: tier-1 `brain.remember`/`brain.forget` skip approval in the cases above (user decision).
- Phase 2's agent committed a gofmt-only change to its own frozen file (`1260c1d`), no semantic change.

**Uncertain**
- Nothing ran against a live gbrain: response shapes, trailing notices, `init` re-run safety.
- Weekly consolidation covers only the day before the run.
- Undo of a merged item forgets only the inbox fact, not the merged page.
- `forget` cannot remove imported wiki pages (gbrain forgets facts by id).
- Small crash windows: lost inbox record, missing outcomes, duplicate digest.

**Frozen tests that were wrong: fixed with your approval on 2026-10-29 (`e38abda`, alone); `make gate` EXIT=0 after**
- M2B-5 `TestM2BScheduledTasksGetNoHistory`: expects 2 messages, the collect step makes it 4.
- `TestM2BMCPToolTrust/untrusted_server_taints_the_task`: the fake MCP tool has no `readOnlyHint`, so it is treated as a write and waits for approval instead of running. Fix: add `"annotations":{"readOnlyHint":true}` to the fake. The "trusted" subtest passes vacuously for the same reason.

**Needs you live**
1. Run `scripts/brain-setup.sh`, add the `gbrain` server to `config/haythos.yaml` (README), start, check for no "mcp server skipped" warning.
2. Ask something from the wiki; say "ingat …" in a thread (no button), then ask from another thread.
3. Leave a thread idle 3 h; next morning check the 07:00 digest and press Undo once.
