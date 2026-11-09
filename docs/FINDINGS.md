# Phase 0 spike findings

The `experiments/...` programs named below were throwaway spikes; they are no longer tracked (last present in commit `888ccfd`).

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

## 5. Drop the Node sidecar: call the `claude` CLI from Go — works (2026-11-09)
Spike: `experiments/claudecli` (own module, stdlib only). CLI 2.1.319 (`~/.local/bin/claude`); sidecar SDK 0.3.310 bundles CLI 2.1.310. Auth: the machine's Claude login, not the vault token.
- **Schema mode** (same contract as today's sidecar): `claude -p --output-format json --tools "" --setting-sources "" --no-session-persistence --strict-mcp-config --system-prompt … --max-turns 2 --json-schema …`, prompt on stdin, temp cwd. Result JSON has `structured_output`. 3.3 / 3.9 / 4.7 s (cold first call ~11 s).
- **Native tool_use mode** (Hermes DirectSDK pattern): tools advertised by a stub MCP stdio server (`--mcp-config`, `--strict-mcp-config`), `--permission-mode dontAsk`, `--max-turns 1`, `--output-format stream-json --verbose`. The assistant event carries a real `tool_use` (`mcp__alfred__vmhealth_read {"host":"alpha"}`); the CLI denies it (`permission_denials`), the stub is never called, run ends `error_max_turns` with **exit status 1**, which must be read as success when a tool batch is present. 2.8 / 3.1 / 3.3 s.
- **Baseline, Agent SDK via sidecar's node_modules**, same prompt: 7.2 / 5.9 / 5.1 s.
- **Model list** without a model turn: stream-json input, send `{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}`; the response has `models[]` (`value`, `displayName`, `description`), what `supportedModels()` returns.
- `--bare` is unusable: it never reads OAuth, only `ANTHROPIC_API_KEY`.
- Open: auth with `CLAUDE_CODE_OAUTH_TOKEN` and a scrubbed env through the CLI directly (Spike 3 covered it via the SDK, which spawns the same CLI); pin the CLI version (the user's install auto-updates); history no longer needs rendering into the prompt: stream-json input replays native frames (see below).
- **Native history replay** (Hermes DirectSDK `prepare_history`): send earlier user frames with `"shouldQuery":false` (each answered by a zero-turn `result`), assistant frames with real `tool_use` blocks, and the last user frame with `tool_result` to query. Tested: prior user + assistant `tool_use mcp__alfred__vmhealth_read` + `tool_result` → the model answered from the tool result, `num_turns` 1, ~9.5 s total including a 4 s sleep in the test.
- Hermes reference: `NousResearch/hermes-official-plugins/claude-subscription-directsdk` (`directsdk.py`, `inert_mcp.py`). Its loopback relay (`admission.py`, `ANTHROPIC_BASE_URL`) rewrites the CLI's outgoing request (cache breakpoints, date line, tool-result restore) and caps it to one upstream request; it is an optimisation, not needed for correctness, and it handles the OAuth bearer, so HaythOS should not copy it. Hermes' built-in Anthropic OAuth provider (`agent/anthropic_adapter.py`) impersonates Claude Code (UA, `x-app: cli`, system prefix, tool renaming) — forbidden by rule 5.

## 6. Other subscriptions as `Model` (2026-11-09)
Spike: `experiments/codexappserver/spike.py` (Python, exploration only; run with an isolated `CODEX_HOME`). Codex CLI 0.166.0, logged in with ChatGPT.
- **Codex `app-server` fits the one-step pattern.** `initialize` with `capabilities.experimentalApi: true`; `thread/start` with `ephemeral`, `approvalPolicy: "never"`, `sandbox: "read-only"`, `baseInstructions` (our system prompt), `dynamicTools: [{type:"function",name,description,inputSchema}]`, and `config.features.*=false` for built-ins (shell_tool, unified_exec, view_image, image_generation, apps, browser_use, computer_use, skill_search, tool_suggest, sleep_tool) plus `web_search: "disabled"`.
  - Tool call: the server sends a JSON-RPC **request** `item/tool/call {callId, tool, arguments}` to the client and waits; we never answer it, we send `turn/interrupt`. 4.5–5.9 s.
  - History replay: `thread/inject_items` (snake_case; `thread/injectItems` is rejected) with raw Responses items (`message`/`input_text`, `function_call`, `function_call_output`), then `turn/start` with `input: []` → the model answered from the tool output. 4.4–5.3 s.
  - **Isolation is mandatory**: with the user's `~/.codex`, a hook/AGENTS file loaded the user's global memory into the turn. With `CODEX_HOME` = a fresh dir holding only `auth.json`, it did not. Codex writes sqlite state/logs into `CODEX_HOME` even with `ephemeral`.
  - Uncertain: whether every built-in tool is off (none was called, not proven); parallel tool calls (server asks one at a time; we interrupt on the first); token usage location in notifications; OpenAI's position on third-party use of ChatGPT-plan auth via the official binary (no explicit statement found; secondhand reports say tolerated / sign-in-with-ChatGPT for partner apps).
- **Hermes reference**: default Codex path calls `chatgpt.com/backend-api/codex` directly with the Codex CLI's OAuth client id (not allowed for us: rule 5 spirit); opt-in `codex_app_server` runtime spawns the official binary but lets Codex own the loop.
- **Gemini CLI subscription**: Gemini CLI terms forbid access via third-party software; 2026 reports of 403 ToS bans for opencode/OpenClaw users. Skip; use a Gemini API key if wanted.
- **Copilot**: `copilot --acp` is official for third-party integration but is an agent (owns its loop) → Builder candidate, not `Model`.
- **Coding plans** (Z.ai GLM, Alibaba, Kimi Code): terms limit keys to interactive coding tools; not for a kernel. Use metered APIs.
- **Cheaper models need no code**: `kind: openai` already takes `base_url` + `secret` (OpenRouter, DeepSeek, Moonshot, Z.ai, DashScope, Groq, Together are OpenAI-compatible with tool calling). Only config + `haythosd auth` secret entries; `/model` switching across providers is not supported yet (selector is claudesdk-only).
- **Better ChatGPT path: official "Sign in with ChatGPT" token sharing (SIWC)** — found in the user's `jaze` project (`internal/chatgpt/client.go`, `docs/VALIDATION-SUBSCRIPTION.md`), docs at https://developers.openai.com/siwc/token-sharing-open-source . App registers itself (`client_id=dynamic_agent_client` + `ext_agent_host_id` → issued `oaiapp_…`), OAuth PKCE on `auth.openai.com` with loopback `127.0.0.1`, scopes `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`, resource `https://api.openai.com/v1`. Inference = public `POST /v1/responses` (`store:false`, `stream:true`, full history in `input`, `instructions` not `system`), native function tools (namespaced). Our own loop, our own identity, no Codex binary, no borrowed client id. Access token 1 h, refresh 30 d and rotates on every refresh (must be serialized and persisted). No device flow: on a VPS, log in locally and copy the credential (`/self-hosted-vms`). Scope of the docs: "open-source and locally hosted apps"; paid/remote apps go through an interest form. jaze verified it against fixtures only — live login and inference are unverified there too. Supersedes the Codex app-server plan.
