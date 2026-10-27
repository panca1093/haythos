# Phase 0 spike findings

Run unattended: no real credentials were used, so live-call items are marked TODO(user).

## 1. modelcontextprotocol/go-sdk — confirmed
- Latest: `v1.8.0` (`go get` output, `experiments/mcp/go.mod`).
- Client transports present and compiling (`experiments/mcp`, `go run .`):
  `mcp.CommandTransport` (stdio subprocess), `mcp.StreamableClientTransport` (HTTP), `mcp.SSEClientTransport` (legacy SSE).

## 2. Claude Agent SDK (TypeScript) — API supports it; live timing TODO(user)
- Package `@anthropic-ai/claude-agent-sdk@0.3.310` (npm, modified 2026-10-23).
- Tool-less single turn: `Options.tools: []` is documented as "Disable all built-in tools" (`sdk.d.ts` ~L1670); MCP servers are only added if passed. Combined with `maxTurns: 1` this gives one assistant turn.
- Structured output: `Options.outputFormat: { type: 'json_schema', schema }` exists (`sdk.d.ts` ~L2000). Design still validates in Go and repairs once, so this is a hint, not trust.
- Per-call overhead: NOT measured. The SDK spawns a Claude Code subprocess per `query()`; `sdk.d.ts` mentions a parked/prewarmed process holding ~230–260 MB. `experiments/agentsdk/probe.mjs` prints wall-clock for 3 calls. TODO(user): run it.
- Not a design blocker.

## 3. Auth via `claude setup-token` — partly verified
- `claude setup-token --help` (CLI 2.1.310): "Set up a long-lived authentication token (requires Claude subscription)".
- `CLAUDE_CODE_OAUTH_TOKEN` is the env var; SDK types reference it as a per-process env (`sdk.d.ts` ~L2843).
- Validity: third-party sources say ~1 year; no official Anthropic page found. A July 2026 bug report claims the token gave 401s in a long-running process while one-shot runs worked. TODO(user): verify lifetime, and that the SDK honours the env token when `~/.claude` credentials also exist (the kernel must run the sidecar with an explicit env, so the latter is controllable).
- Sources: https://gist.github.com/coenjacobs/d37adc34149d8c30034cd1f20a89cce9 , https://claudeissues.com/issue/78181-setup-token-claude-code-oauth-token-fails-with-401-in-long-running-claude-channe

## 4. Go ACP client library — exists (report only, ACP is M3)
- `github.com/coder/acp-go-sdk`, latest listed `v0.13.5` (`go list -m -versions`).
- Agent package on npm: `@zed-industries/claude-agent-acp@0.23.1` and `@agentclientprotocol/claude-agent-acp@0.97.0` both resolve; TODO(M3): decide which is canonical.
