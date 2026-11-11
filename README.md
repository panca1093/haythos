# HaythOS

Personal agentic OS. Go kernel, persona "Alfred". Architecture and rules: `CLAUDE.md`. Milestone plans and run reports: `docs/` (the full design spec is kept outside this public repo).

## Run locally (M1)

Requirements: Go (version in `go.mod`), the official `claude` CLI for the Claude subscription model, `golangci-lint` v2.

```bash
export HAYTHOS_VAULT_KEY=$(openssl rand -base64 32)   # keep this; without it the vault cannot be read (a service reads it from the OS keychain, see below)
```

Your config is a local copy of the example; git ignores it, so your ids and local servers never get committed:

```bash
cp config/haythos.example.yaml config/haythos.yaml
```

### `haythos setup` (Claude subscription, recommended)

One command onboards `claude-sub` (kind `claudesdk`). Needs the official `claude` CLI on `PATH` (or `claude_cli.command` in the config) and `HAYTHOS_VAULT_KEY` set.

```bash
go run ./cmd/haythos setup
```

1. Runs `claude setup-token` (browser login). The long-lived token it prints is stored in the vault as `secret://claude`; the console shows `[stored in vault]` instead of the token.
2. Runs the `claude` CLI with that token and fetches its live model list through the `initialize` handshake (no model turn is sent).
3. Asks you to pick a model by number.
4. Writes `default_model: claude-sub` and `models.claude-sub.model: <your pick>` into `config/haythos.yaml`, keeping comments. Blank lines and flow-style spacing in that file may be normalised.

If the token cannot be found in the CLI output, use the manual route below. If only the model list fails, the token stays stored; re-run `setup`.

### `haythos auth` (manual fallback)

Secrets go into an encrypted file vault (`data/vault.enc`, AES-256-GCM, key from `HAYTHOS_VAULT_KEY`). Apart from `auth chatgpt` (below), the kernel never does OAuth; you paste what each provider gives you. Input is hidden.

```bash
go run ./cmd/haythos auth claude      # paste the token printed by `claude setup-token` (same secret `setup` fills)
go run ./cmd/haythos auth anthropic   # Anthropic API key (Messages API)
go run ./cmd/haythos auth openai      # key for an OpenAI-compatible endpoint
go run ./cmd/haythos auth discord     # Discord bot token
```

Then edit `config/haythos.yaml`: set `discord.owner_id` and `default_model`.

### `haythos auth chatgpt` (ChatGPT plan)

Runs Alfred on your ChatGPT plan through OpenAI's official [Sign in with ChatGPT token sharing](https://developers.openai.com/siwc/token-sharing-open-source) for open-source apps. HaythOS registers as its own app ("HaythOS"); it does not reuse Codex credentials.

```bash
go run ./cmd/haythos auth chatgpt
```

1. Prints a `http://127.0.0.1:<port>/login/...` link and tries to open your browser. Sign in and allow ChatGPT plan usage.
2. The access, refresh and ID tokens, the issued client id and this host's id are stored in the vault as `secret://chatgpt`; nothing is printed.
3. Add a model in `config/haythos.yaml`, for example `chatgpt-sub: { kind: chatgpt, model: gpt-5 }` (`model` is optional; empty uses the first model your account lists), and point `default_model` or a job's `model:` at it.

Alfred refreshes the token itself (1 h access token; the 30-day refresh token rotates on every refresh and is written to the vault before use). When the plan's usage limit is hit the task fails with a link to https://chatgpt.com/settings/usage; it never falls back to API-key billing. Run `auth chatgpt` again if the refresh token expires or is revoked.

Headless server (VPS): the login callback goes to `127.0.0.1` on the machine with the browser, so there is no device flow. Sign in on your laptop, then copy the vault file (`data/vault.enc`) and keep the same `HAYTHOS_VAULT_KEY` on the server, over a secure channel. The server then owns later refreshes; stop using the copy on the laptop, or the two will fight over the rotating refresh token.

### Start

```bash
go run ./cmd/haythos              # serve: Discord + scheduler + the client socket
go run ./cmd/haythos run-job vm-health   # one-shot manual trigger, prints the report to stdout
```

Only one `haythos` can use the database at a time (`<db>.lock`), so stop `serve` before `run-job`. `run-job` runs only that job; unfinished tasks wait for the next `serve`.

### Terminal chat (`haythos chat`)

One binary, `haythos`. The kernel (`haythos serve`) keeps Discord and the scheduler running and opens one WebSocket on a unix socket, `<home>/run/haythos.sock`, where `<home>` is `$HAYTHOS_HOME` or `~/.haythos` (directory `0700`, socket `0600`). Every terminal client talks to the kernel through that one socket; there is no TCP listener.

```bash
go install ./cmd/haythos            # chat starts the kernel from this binary, so build it (go run uses a temp file)
haythos chat                        # new conversation tui:<12 hex>; starts the kernel in the background if none answers
haythos chat --resume               # continue the most recent terminal conversation
haythos chat --session 6836b54688f8 # continue a given one (ids from `haythos sessions`)
haythos sessions                    # <id>  <last active>  <first line>, newest first
haythos status                      # running pid <n> since <time> version <v> channels <list>, or not running
haythos stop                        # SIGTERM to the kernel, waits up to 10 s
```

Run these from the directory that holds `config/` (or pass `-config`, `-policy`, `-jobs` before the subcommand); a kernel started by `chat` gets the same files as absolute paths and logs to `<home>/logs/haythos.log`.

- Enter sends; `/quit` or Ctrl+C closes the window only. The kernel keeps running and a running task keeps going.
- An approval for this conversation shows inline with tool, target and preview; press `y` or `n`. Approvals stay where they were asked: a Discord approval never shows in the terminal and the reverse, and a terminal client can only act on its own `tui:` conversation.
- Close the window while Alfred waits for your approval and the task stays waiting; `haythos chat --resume` shows the approval again.
- After rebuilding the binary the old kernel keeps running; the chat says so in one line. Run `haythos stop` and start `chat` again.
- PgUp/PgDn scroll the history.

### Run the kernel as a service

```bash
haythos service install     # macOS: launchd agent; Linux: systemd user unit (haythos.service)
haythos service uninstall
```

The service starts the kernel at login and restarts it when it dies (on macOS also after `haythos stop`; use `service uninstall` to stop it for good). It runs `haythos <same flags> serve` from the directory you install from, logging to `<home>/logs/haythos.log`. A service gets no environment, so the vault key comes from the OS keychain (macOS keychain, or Secret Service via `secret-tool` on Linux; service `haythos`, account `vault-key`). `install` offers to copy `HAYTHOS_VAULT_KEY` there once; the key is never written into the plist or unit file. When `HAYTHOS_VAULT_KEY` is set it always wins over the keychain. `install` stops a kernel you started by hand first, so the service owns the socket. The service PATH is minimal: if `claude` lives in Homebrew, set `claude_cli.command` to its absolute path.

### Chat with a kernel on another machine

There is no network listener. Forward the socket over SSH and point the client at it with `HAYTHOS_HOME`:

```bash
mkdir -p ~/.haythos-vps/run && chmod 700 ~/.haythos-vps/run
ssh -N -o StreamLocalBindUnlink=yes -L $HOME/.haythos-vps/run/haythos.sock:/home/<user>/.haythos/run/haythos.sock vps
HAYTHOS_HOME=~/.haythos-vps haythos chat    # in another terminal, while the tunnel is up
```

Start the tunnel first: with nothing answering on that socket, `chat` would start a local kernel instead.

Discord: only `discord.owner_id` is obeyed, everywhere; everyone else is ignored. Alfred answers like this:

- **DM**: every message is handled.
- **Server channel**: only a message that @mentions the bot. Alfred starts a thread on it and replies there. The bot needs View Channel, Send Messages, Create Public Threads and Send Messages in Threads.
- **Thread**: every message is handled once the thread is Alfred's (he started it, or you @mentioned him in it). `/mention-only` inside a thread toggles whether he then answers only when mentioned; it replies privately with the new state. Both the joined threads and the toggle survive restarts.

Posting: ask Alfred to "post X in #infra" (or in a thread, or to your DM). He looks the channel up with `discord.channels`, then calls `discord.post`, a tier 2 write that always asks for your approval with the channel and text shown. Only server text channels, news channels and threads the bot can see, or your own DM, are accepted; anything else is refused and nothing is sent. Long text is split into several messages, and mentions in the text never ping anyone. `discord.post` is for chat only; scheduled jobs cannot use it.

Alfred replies (and asks for approval) where you wrote. `/pause` stops every write immediately (also those already approved and not yet dispatched) and survives restarts; `/resume` lifts it.

### Prompt cache on `claude-sub`

Within one task, each step resumes the same `claude` CLI session and sends only the new text, so the CLI's prompt cache covers everything the earlier steps already sent. The CLI keeps those session transcripts (conversation text) in `claude-cli/<model name>/` next to the database (`data/claude-cli/claude-sub/` by default); at most 8 per model are kept, and all are cleared when the kernel starts. A new chat message starts a new session.

### Switching the model

Jobs pick a model by name (`model:` in `jobs/*.yaml`, else `default_model`). The same job runs on `claude-api` (Messages API), `claude-sub` (`claude` CLI) or `openai` by changing only that name. `kind: claudesdk` accepts an optional `model:`; empty uses the CLI default. `haythos setup` sets it for you from the live CLI list. Each step runs `claude -p` once in a fresh temp dir with no built-in tools, no settings, no MCP servers and no session persistence. While `serve` runs, `/model` in Discord shows the same live list and switches the model without a restart. This works only when `default_model` is a `claudesdk` model.

### Runtime settings

`config/haythos.yaml` holds the defaults. Changes made while running (today: `/model`) are stored as events in the database, so they survive restarts, keep a history, and never rewrite the config file. `/settings` in Discord shows each value and whether it is the default or was changed.

### Scheduled jobs from chat

Ask Alfred in Discord, for example "every Monday 09:00 check the disk and report". Alfred proposes a job (name, cron, goal, tools) and you approve it with a button; nothing is scheduled before that. Disable, enable, change or delete it the same way. `/jobs` lists every job with its source (`config` for `jobs/*.yaml`, `chat` for these) and its next run.

Where a job reports: `deliver_to` is a list of Discord channel or thread IDs, or `dm`. When you create a job from chat without saying where, it reports where you asked for it (that thread or your DM); say "report in #infra" and Alfred looks the channel up with the `discord.channels` tool. `jobs/*.yaml` jobs accept `deliver_to` too. With no target a job reports in your DM. A target that is not a channel or thread the bot can see in a server falls back to your DM with a note. `/jobs` shows each job's targets.

Rules: cron is Asia/Jakarta unless it starts with `CRON_TZ=`; tools must already exist and be tier 0 or 1; a job cannot create jobs; budget defaults to 200k tokens / 5 steps (max 1M / 10); names of `jobs/*.yaml` jobs are reserved. Re-enabling a job does not replay the runs it missed.

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
make gate    # gofmt, go vet, golangci-lint, go test -race
```

`tests/acceptance/` holds the acceptance tests (including the frozen milestone contract tests), grouped by use case (approval, ledger, taint, task lifecycle, settings, jobs, conversation, brain). Existing assertions are never edited to make a test pass; new tests go in the file for their use case.

## Repo layout

| Folder | What it is |
| --- | --- |
| `cmd/haythos/` | The `haythos` binary: CLI subcommands (`serve`, `chat`, `sessions`, `status`, `stop`, `service`, `run-job`, `auth`, `setup`) and wiring of everything below |
| `core/` | Pure rules with no I/O: task state machine, policy tiers, output contract |
| `app/` | Use cases (run a task, handle approvals, the scheduler tick) and the interfaces they need (`ports.go`) |
| `app/gateway/` | The only path a tool call can take: policy, approval, ledger, secrets, execute |
| `adapters/` | Real implementations of those interfaces: model providers, Discord, SQLite, tools, vault |
| `adapters/channel/ws/` | The one WebSocket client API on the kernel's unix socket (server and client) |
| `adapters/tui/` | The terminal chat screen (Bubble Tea) |
| `adapters/service/` | launchd plist and systemd unit generation, `service install`/`uninstall` |
| `adapters/vault/keychain/` | Reads and stores the vault key in the OS keychain |
| `adapters/model/chatgpt/` | ChatGPT plan model: Sign in with ChatGPT (browser login, token refresh) and one step on the Responses API |
| `adapters/config/` | Go code that loads and validates the YAML in `config/`, and saves the model choice back to it |
| `adapters/testfakes/` | In-memory fakes of the interfaces, for tests only |
| `config/` | The YAML files you edit: `haythos.yaml` (your untracked copy of `haythos.example.yaml`), `policy.yaml` |
| `jobs/` | Job definitions (what to do, which tools, budget, schedule) |
| `skills/` | Markdown instructions the model reads (how to do it) |
| `tests/acceptance/` | Whole-kernel tests with fakes, one file per use case plus `harness_test.go`; the gate for every milestone. Never delete |
| `docs/` | Notes for humans: milestone kickoffs, plans, run reports, spike findings (`FINDINGS.md`) |
