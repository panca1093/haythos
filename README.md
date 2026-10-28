# HaythOS

Personal agentic OS. Go kernel, persona "Alfred". Architecture and rules: `CLAUDE.md`. Spec, milestone plans and run reports: `docs/`.

## Run locally (M1)

Requirements: Go (version in `go.mod`), Node 22+ for the Claude subscription sidecar, `golangci-lint` v2.

```bash
export HAYTHOS_VAULT_KEY=$(openssl rand -base64 32)   # keep this; without it the vault cannot be read
```

### `haythosd setup` (Claude subscription, recommended)

One command onboards `claude-sub` (kind `claudesdk`). Needs the official `claude` CLI on `PATH`, the sidecar installed (`cd sidecar/claude-sdk && npm install`) and `HAYTHOS_VAULT_KEY` set.

```bash
go run ./cmd/haythosd setup
```

1. Runs `claude setup-token` (browser login). The long-lived token it prints is stored in the vault as `secret://claude`; the console shows `[stored in vault]` instead of the token.
2. Starts the sidecar with that token and fetches the live model list from the Agent SDK (no model turn is sent).
3. Asks you to pick a model by number.
4. Writes `default_model: claude-sub` and `models.claude-sub.model: <your pick>` into `config/haythos.yaml`, keeping comments. Blank lines and flow-style spacing in that file may be normalised.

If the token cannot be found in the CLI output, use the manual route below. If only the model list fails, the token stays stored; re-run `setup`.

### `haythosd auth` (manual fallback)

Secrets go into an encrypted file vault (`data/vault.enc`, AES-256-GCM, key from `HAYTHOS_VAULT_KEY`). The kernel never does OAuth; you paste what each provider gives you. Input is hidden.

```bash
go run ./cmd/haythosd auth claude      # paste the token printed by `claude setup-token` (same secret `setup` fills)
go run ./cmd/haythosd auth anthropic   # Anthropic API key (Messages API)
go run ./cmd/haythosd auth openai      # key for an OpenAI-compatible endpoint
go run ./cmd/haythosd auth discord     # Discord bot token
```

Then edit `config/haythos.yaml`: set `discord.owner_id` and `default_model`. (`discord.channel_id` is gone; if your file still has it, Alfred ignores it with a warning, so delete it.) Install the sidecar once: `cd sidecar/claude-sdk && npm install`.

### Start

```bash
go run ./cmd/haythosd              # serve: Discord + scheduler
go run ./cmd/haythosd run-job vm-health   # one-shot manual trigger, prints the report to stdout
```

Only one `haythosd` can use the database at a time (`<db>.lock`), so stop `serve` before `run-job`. `run-job` runs only that job; unfinished tasks wait for the next `serve`.

Discord: only `discord.owner_id` is obeyed, everywhere; everyone else is ignored. Alfred answers like this:

- **DM**: every message is handled.
- **Server channel**: only a message that @mentions the bot. Alfred starts a thread on it and replies there. The bot needs View Channel, Send Messages, Create Public Threads and Send Messages in Threads.
- **Thread**: every message is handled once the thread is Alfred's (he started it, or you @mentioned him in it). `/mention-only` inside a thread toggles whether he then answers only when mentioned; it replies privately with the new state. Both the joined threads and the toggle survive restarts.

Posting: ask Alfred to "post X in #infra" (or in a thread, or to your DM). He looks the channel up with `discord.channels`, then calls `discord.post`, a tier 2 write that always asks for your approval with the channel and text shown. Only server text channels, news channels and threads the bot can see, or your own DM, are accepted; anything else is refused and nothing is sent. Long text is split into several messages, and mentions in the text never ping anyone. `discord.post` is for chat only; scheduled jobs cannot use it.

Alfred replies (and asks for approval) where you wrote. `/pause` stops every write immediately (also those already approved and not yet dispatched) and survives restarts; `/resume` lifts it.

### Switching the model

Jobs pick a model by name (`model:` in `jobs/*.yaml`, else `default_model`). The same job runs on `claude-api` (Messages API), `claude-sub` (sidecar) or `openai` by changing only that name. `kind: claudesdk` accepts an optional `model:`; empty uses the SDK default. `haythosd setup` sets it for you from the live SDK list. While `serve` runs, `/model` in Discord shows the same live list and switches the model without a restart. This works only when `default_model` is a `claudesdk` model.

### Runtime settings

`config/haythos.yaml` holds the defaults. Changes made while running (today: `/model`) are stored as events in the database, so they survive restarts, keep a history, and never rewrite the config file. `/settings` in Discord shows each value and whether it is the default or was changed.

### Scheduled jobs from chat

Ask Alfred in Discord, for example "every Monday 09:00 check the disk and report". Alfred proposes a job (name, cron, goal, tools) and you approve it with a button; nothing is scheduled before that. Disable, enable, change or delete it the same way. `/jobs` lists every job with its source (`config` for `jobs/*.yaml`, `chat` for these) and its next run.

Where a job reports: `deliver_to` is a list of Discord channel or thread IDs, or `dm`. When you create a job from chat without saying where, it reports where you asked for it (that thread or your DM); say "report in #infra" and Alfred looks the channel up with the `discord.channels` tool. `jobs/*.yaml` jobs accept `deliver_to` too. With no target a job reports in your DM. A target that is not a channel or thread the bot can see in a server falls back to your DM with a note. `/jobs` shows each job's targets.

Rules: cron is Asia/Jakarta unless it starts with `CRON_TZ=`; tools must already exist and be tier 0 or 1; a job cannot create jobs; budget defaults to 20k tokens / 5 steps (max 50k / 10); names of `jobs/*.yaml` jobs are reserved. Re-enabling a job does not replay the runs it missed.

### Brain (gbrain)

Long-term memory is [gbrain](https://github.com/garrytan/gbrain), run as an MCP stdio child (`gbrain serve --surface verbs`), keyless: no embedding key, nothing leaves the machine except the prompt to the model.

```bash
scripts/brain-setup.sh   # you run it: pins gbrain, `gbrain init --pglite --no-embedding`, imports the wiki folders
```

It imports only `entities/ concepts/ tickets/ learnings/` and `index.md` from `~/Documents/MyBook/wiki` (override with `WIKI_DIR`); never `personal/`. Then declare the server in `config/haythos.yaml`. A server named `gbrain` also becomes Alfred's brain:

```yaml
mcp:
  servers:
    - name: gbrain
      command: gbrain
      args: [serve, --surface, verbs]
      trusted: true   # omit for any other server: its tool results then taint the task
```

- **Brain first**: before the first model step of a chat task the kernel recalls with your message (at most 3 hits, about 3 KB) and injects them once as a labelled data block. No hits, nothing injected.
- **Tools**: `brain.recall` (tier 0), `brain.remember` and `brain.forget` (tier 1). Say "ingat ..." and `brain.remember` runs without a button, but only when the task is untainted and started by your own message (`auto_when_clean` in `config/policy.yaml`); a tainted task asks for approval. The stored provenance (`owner/<thread>`) is set by the kernel, never by the model.
- **Other MCP servers** use the same `mcp.servers` list, are untrusted by default, and need their tools listed in `config/policy.yaml` and a job's `tools` before the model can call them.
- Zero-width and bidi control characters are stripped from recalled content and untrusted tool output.

### Brain upkeep (M2B)

Needs the brain wired (Phase 2); all three steps are silent unless noted.

- **Stale hook.** A conversation idle for 3 h with turns not yet extracted gets one background task (`auto/<conversation>` source, unverified) that saves decisions, preferences, corrections, lessons and facts about people/projects/repos into the brain inbox (`inbox/...`). A conversation in which any task read untrusted content is never extracted; it is listed as skipped in the digest.
- **02:00 consolidation** (`jobs/brain-consolidate.yaml`, Asia/Jakarta). Processes only the inbox items saved on the previous Jakarta day: merge into existing pages, drop noise. Sends no message. If nothing was saved, no task runs.
- **07:00 digest** (`jobs/brain-digest.yaml`). One message listing what was saved or merged, with an Undo button per item. Ignore it and everything stays. Undo forgets that item (a second click does nothing). No new items and no skipped conversations: no message.
- **Weekly instead of daily.** Edit `trigger.cron` in `jobs/brain-consolidate.yaml` (for example `CRON_TZ=Asia/Jakarta 0 2 * * 1`) and restart; `/jobs` then shows the next run. The job reads only the day before it fires, so weekly means only that Sunday's items are consolidated; items from other days stay in the inbox.

## Development

```bash
make gate    # gofmt, go vet, golangci-lint, go test -race, sidecar typecheck + tests
```

`tests/acceptance/m1_test.go` holds the frozen M1 contract tests; never edit it to make a test pass.

## Repo layout

| Folder | What it is |
| --- | --- |
| `cmd/haythosd/` | The `haythosd` binary: CLI subcommands (`serve`, `run-job`, `auth`, `setup`) and wiring of everything below |
| `core/` | Pure rules with no I/O: task state machine, policy tiers, output contract |
| `app/` | Use cases (run a task, handle approvals, the scheduler tick) and the interfaces they need (`ports.go`) |
| `app/gateway/` | The only path a tool call can take: policy, approval, ledger, secrets, execute |
| `adapters/` | Real implementations of those interfaces: model providers, Discord, SQLite, tools, vault |
| `internal/config/` | Go code that loads and validates the YAML in `config/`, and saves the model choice back to it |
| `internal/testfakes/` | In-memory fakes of the interfaces, for tests only |
| `config/` | The YAML files you edit: `haythos.yaml`, `policy.yaml` |
| `jobs/` | Job definitions (what to do, which tools, budget, schedule) |
| `skills/` | Markdown instructions the model reads (how to do it) |
| `sidecar/claude-sdk/` | TypeScript helper that calls the Claude Agent SDK: one step per call, plus the live model list |
| `tests/acceptance/` | Whole-kernel tests with fakes; the gate for every milestone. Never delete |
| `docs/` | Notes for humans: spec, milestone kickoffs, run reports |
| `experiments/` | Throwaway programs from M1 Phase 0 and their findings; not part of the product. Delete once open items in `docs/REPORT.md` are done |
