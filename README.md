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

Then edit `config/haythos.yaml`: set `discord.owner_id` and `discord.channel_id`, and `default_model`. Install the sidecar once: `cd sidecar/claude-sdk && npm install`.

### Start

```bash
go run ./cmd/haythosd              # serve: Discord + scheduler
go run ./cmd/haythosd run-job vm-health   # one-shot manual trigger, prints the report to stdout
```

Only one `haythosd` can use the database at a time (`<db>.lock`), so stop `serve` before `run-job`. `run-job` runs only that job; unfinished tasks wait for the next `serve`.

Discord: only `discord.owner_id` in `discord.channel_id` is obeyed. `/pause` stops every write immediately (also those already approved and not yet dispatched) and survives restarts; `/resume` lifts it.

### Switching the model

Jobs pick a model by name (`model:` in `jobs/*.yaml`, else `default_model`). The same job runs on `claude-api` (Messages API), `claude-sub` (sidecar) or `openai` by changing only that name. `kind: claudesdk` accepts an optional `model:`; empty uses the SDK default. `haythosd setup` sets it for you from the live SDK list. While `serve` runs, `/model` in Discord shows the same live list and switches the model without a restart; the choice is saved to `config/haythos.yaml`. This works only when `default_model` is a `claudesdk` model.

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
