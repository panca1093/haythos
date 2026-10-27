# HaythOS

Personal agentic OS. Go kernel, persona "Alfred". Architecture and rules: `CLAUDE.md`. Milestone plan: `M1_KICKOFF.md`.

## Run locally (M1)

Requirements: Go (version in `go.mod`), Node 22+ for the Claude subscription sidecar, `golangci-lint` v2.

```bash
export HAYTHOS_VAULT_KEY=$(openssl rand -base64 32)   # keep this; without it the vault cannot be read
```

### `haythosd auth`

Secrets go into an encrypted file vault (`data/vault.enc`, AES-256-GCM, key from `HAYTHOS_VAULT_KEY`). The kernel never does OAuth; you paste what each provider gives you. Input is hidden.

```bash
go run ./cmd/haythosd auth claude      # paste the token printed by `claude setup-token` (subscription, via the Agent SDK sidecar)
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

Jobs pick a model by name (`model:` in `jobs/*.yaml`, else `default_model`). The same job runs on `claude-api` (Messages API), `claude-sub` (sidecar) or `openai` by changing only that name.

## Development

```bash
make gate    # gofmt, go vet, golangci-lint, go test -race, sidecar typecheck + tests
```

`tests/acceptance/m1_test.go` holds the frozen M1 contract tests; never edit it to make a test pass.

## Repo layout

| Folder | What it is |
| --- | --- |
| `cmd/haythosd/` | The `haythosd` binary: CLI subcommands and wiring of everything below |
| `core/` | Pure rules with no I/O: task state machine, policy tiers, output contract |
| `app/` | Use cases (run a task, handle approvals, the scheduler tick) and the interfaces they need (`ports.go`) |
| `app/gateway/` | The only path a tool call can take: policy, approval, ledger, secrets, execute |
| `adapters/` | Real implementations of those interfaces: model providers, Discord, SQLite, tools, vault |
| `internal/config/` | Go code that loads and validates the YAML in `config/` |
| `internal/testfakes/` | In-memory fakes of the interfaces, for tests only |
| `config/` | The YAML files you edit: `haythos.yaml`, `policy.yaml` |
| `jobs/` | Job definitions (what to do, which tools, budget, schedule) |
| `skills/` | Markdown instructions the model reads (how to do it) |
| `sidecar/claude-sdk/` | TypeScript helper that calls the Claude Agent SDK, one step per call |
| `tests/acceptance/` | Whole-kernel tests with fakes; the gate for every milestone. Never delete |
| `experiments/` | Throwaway programs from M1 Phase 0 and their findings; not part of the product. Delete once open items in `REPORT.md` are done |
