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

Discord: only `discord.owner_id` in `discord.channel_id` is obeyed. `/pause` stops every write immediately (also those already approved and not yet dispatched) and survives restarts; `/resume` lifts it.

### Switching the model

Jobs pick a model by name (`model:` in `jobs/*.yaml`, else `default_model`). The same job runs on `claude-api` (Messages API), `claude-sub` (sidecar) or `openai` by changing only that name.

## Development

```bash
make gate    # gofmt, go vet, golangci-lint, go test -race, sidecar typecheck + tests
```

`acceptance/m1_test.go` holds the frozen M1 contract tests; never edit it to make a test pass.
