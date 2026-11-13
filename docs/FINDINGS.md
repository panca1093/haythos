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
- **Cheaper models need no code**: `kind: openai` already takes `base_url` + `secret` (OpenRouter, DeepSeek, Moonshot, Z.ai, DashScope, Groq, Together are OpenAI-compatible with tool calling). Only config + `haythos auth` secret entries; `/model` switching across providers is not supported yet (selector is claudesdk-only).
- **Better ChatGPT path: official "Sign in with ChatGPT" token sharing (SIWC)** — found in the user's `jaze` project (`internal/chatgpt/client.go`, `docs/VALIDATION-SUBSCRIPTION.md`), docs at https://developers.openai.com/siwc/token-sharing-open-source . App registers itself (`client_id=dynamic_agent_client` + `ext_agent_host_id` → issued `oaiapp_…`), OAuth PKCE on `auth.openai.com` with loopback `127.0.0.1`, scopes `openid profile email offline_access resource.invoke chatgpt.tokens.use.direct`, resource `https://api.openai.com/v1`. Inference = public `POST /v1/responses` (`store:false`, `stream:true`, full history in `input`, `instructions` not `system`), native function tools (namespaced). Our own loop, our own identity, no Codex binary, no borrowed client id. Access token 1 h, refresh 30 d and rotates on every refresh (must be serialized and persisted). No device flow: on a VPS, log in locally and copy the credential (`/self-hosted-vms`). Scope of the docs: "open-source and locally hosted apps"; paid/remote apps go through an interest form. jaze verified it against fixtures only — live login and inference are unverified there too. Supersedes the Codex app-server plan.

## 7. Prompt cache on the `claude -p` path (2026-11-11)
Measured with CLI 2.1.322: token usage from real calls, and request bodies recorded on a local listener that kept no headers and forwarded nothing.
- **Symptom.** 21 `claude-sub` calls since the context change read 9.6% of input from cache; `cache_read` was a constant ~634 tokens (the system blocks), however long the prompt.
- **What the CLI sends.** `cache_control` sits only on its two system blocks and on a trailing `role: system` "# Environment" message that names the working directory. The user message has no breakpoint. An identical prompt reads ~97% from cache; a prompt that only appends reads 0%.
- **Not fixes (all read 0%).** One stream-json user frame with a text block per history message. Separate user/assistant frames with `shouldQuery:false`: the CLI puts the assistant frame first and merges the user frames into one message, so the "native history replay" of §5 is not order-safe for text history. Also, `--input-format stream-json` requires `--output-format stream-json`.
- **Fix.** Keep session persistence, run in a fixed working directory, start with `--session-id <uuid>` and send only the new text with `--resume <uuid>`: 97-98% read across separate processes. Two user frames inside one live process hit as well (not used: it needs long-lived processes).
- **Isolation.** `CLAUDE_CONFIG_DIR` moves all CLI state, transcripts included (`projects/<cwd slug>/<id>.jsonl`), into a directory the kernel owns; the machine's Claude login is not visible there, the vault token in `CLAUDE_CODE_OAUTH_TOKEN` authenticates.
- **Live check through the kernel** (three-step job, real token, temporary database): input 5264 / 5847 / 6450 tokens, cache read 0 / 5262 / 5843 (0% / 89% / 90%).
- **Across messages** (same day): a follow-up message now replays the earlier task's own messages unchanged (M2B-1 amended), so its first step extends the last prompt of the previous message. Live check, two messages with a file read: 53% / 55% on the first message, **94%** on the second message's first step.
- **Still open.** The CLI adds the Claude account's email to every prompt in its own context block.

## 8. Runtime spike (2026-11-13)
Phase 0 of `docs/plans/2026-11-13-runtime-acp.md`. Code: `tests/spike/` (`//go:build integration`; `SPIKE_OUT=<dir>` writes the full frame transcript per test). Run: `go test -tags integration ./tests/spike/ -run 'TestOpencodeACP|TestClaude|TestCodex' -v`. Versions: opencode 2.1.3, claude 2.1.323 (haiku), codex-cli 0.166.0. Every runtime ran in a fresh `git init` scratch repo; child env was PATH, HOME, HAYTHOS_TASK, HAYTHOS_SESSION plus **USER** (see surprises). Prompts: append a line to README, then `rm victim.txt` with the answer denied.

### opencode (`opencode acp`)
Newline-delimited JSON-RPC 2.0 on stdio. Trimmed transcript:
```
-> initialize {protocolVersion:1, clientCapabilities:{fs:{readTextFile:false,writeTextFile:false}, terminal:false}}
<- result {protocolVersion:1, agentInfo:{name:"OpenCode",version:"2.1.3"}, authMethods:[{id:"opencode-login"}],
           agentCapabilities:{loadSession:true, promptCapabilities:{image:true,embeddedContext:true},
             mcpCapabilities:{http:true,sse:false}, sessionCapabilities:{fork,list,resume,close,delete,additionalDirectories}}}
-> session/new {cwd, mcpServers:[]}
<- result {sessionId:"ses_…", configOptions:[{id:"model",category:"model",type:"select",currentValue,options:[…]},
                                             {id:"mode",category:"mode",currentValue:"build",options:[build,…]}]}
-> session/set_model                 <- error -32601 Method not found
-> session/set_config_option {sessionId, configId:"model", value:"opencode/deepseek-v4.1-flash-free"}   <- result {configOptions}
-> session/prompt {sessionId, prompt:[{type:"text",text:"Append the line …"}]}
<- session/update agent_thought_chunk | tool_call {kind:"execute",status:"pending"} | tool_call_update {status:"in_progress"|"completed"|"failed", title:"printf … >> README.md", rawInput}
<- session/update agent_message_chunk {messageId, content:{type:"text",text:"done"}} | usage_update {used,size,cost} | available_commands_update
<- result {stopReason:"end_turn", usage:{inputTokens:7741,outputTokens:133,cachedReadTokens:19840,totalTokens:27737}}
```
- Stop reasons seen: `end_turn`; **`cancelled` after a rejected permission** (the whole turn ends, the model gets no chance to explain).
- Guarded action: the agent sends a **request** `session/request_permission {sessionId, toolCall:{toolCallId,name:"shell",title:"rm victim.txt",kind:"execute",rawInput:{command,cwd}}, options:[{optionId:"once",kind:"allow_once"},{optionId:"always",kind:"allow_always"},{optionId:"reject",kind:"reject_once"}]}`; the reply is `{outcome:{outcome:"selected",optionId:"reject"}}`. Rejected: `victim.txt` survived, `tool_call_update status:"failed"`, then `stopReason:"cancelled"`. The model's own update of the title only carries the command once the tool is `in_progress`.
- **Guard config:** yes, with config only. `OPENCODE_CONFIG_CONTENT` (JSON, read from the env) with `permission.bash: {"*":"allow","rm *":"ask","git reset --hard*":"ask","ssh *":"ask","sudo *":"ask","git push --force*":"ask"}`, `edit:"allow"`, `read:{"*":"allow","*.env":"ask"}`. The README append ran with no prompt; `rm victim.txt` asked. Not verified: that a compound `cd x && rm y` or a `cat .env` through bash is caught (only `rm` and the plain append were probed); the `read` ask rule for `.env` and `~/.ssh` was not exercised; the opencode default with no config was not run.
- Spec check (agentclientprotocol.com, 2026-11-13): docs are under `protocol/v1`; baseline methods `initialize`, `authenticate`, `session/new`, `session/prompt`, `session/cancel`, `session/request_permission`; optional `session/load`, `session/set_mode`. The docs say `session/set_mode` "will be removed in a future version" in favour of Session Config Options, and `session/set_model` is not in the docs. opencode agrees: **`session/set_model` does not exist, model and mode are `configOptions` set through `session/set_config_option`** (opencode returns the full `configOptions` list again).
- Auth: the default model (`opencode/gemini-4-hydrogen`) failed with `-32603 provider.quota` (Console balance) and `openai/*` with `-32000 Authentication required`. A free zen model worked. A deepseek key lives in the owner's env; it was deliberately not forwarded into the child env. Auth failures come back as an error response to `session/prompt`, not a stop reason.

### claude (stream-json both ways)
Args: `-p --input-format stream-json --output-format stream-json --verbose --permission-prompt-tool stdio --model haiku --setting-sources "" --no-session-persistence --settings '<json>'`. `--permission-prompt-tool` is accepted although it is not in `--help` (help lists `--permission-prompts host|none`, default `host`).
```
-> {"type":"control_request","request_id":"init-1","request":{"subtype":"initialize"}}
<- {"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{"commands":[…],"models":[…]}}}
-> {"type":"user","message":{"role":"user","content":[{"type":"text","text":"Run `rm victim.txt` …"}]}}
<- {"type":"system","subtype":"init","session_id":…,"tools":[…]}            (arrives after the first user message, not after initialize)
<- {"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm victim.txt"}}]}}
<- {"type":"control_request","request_id":"9279…","request":{"subtype":"can_use_tool","tool_name":"Bash","display_name":"Bash",
     "input":{"command":"rm victim.txt","description":"…"},"decision_reason_type":"rule","decision_reason_rule_source":"flagSettings","tool_use_id":"toolu_…"}}
-> {"type":"control_response","response":{"subtype":"success","request_id":"9279…","response":{"behavior":"deny","message":"denied by spike"}}}
<- {"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","terminal_reason":"completed","num_turns":2,"result":"The command was denied…"}
```
- Allow reply: `{"behavior":"allow","updatedInput":<the input>}`. Result frames seen: `subtype:success, stop_reason:end_turn, terminal_reason:completed`; after an interrupt `subtype:error_during_execution, is_error:true, stop_reason:tool_use, terminal_reason:aborted_streaming`.
- **Deny keeps the turn going**: the model sees the denial message and answers (unlike opencode's `cancelled`). A denied turn is a normal `success` result, so the contract (diff present) is what has to catch it.
- Interrupt: `{"type":"control_request","request_id":"int-1","request":{"subtype":"interrupt"}}` → `control_response {still_queued:[]}`, a `user` frame "[Request interrupted by user]", then the `result` above. The process stays alive and the stdin loop is still usable (a `commands_changed` system frame follows). Caveat: the test raced a tool the CLI blocked on its own (standalone `sleep 60`), so interrupt of a long-running tool was not isolated.
- **Open question 1 (guard translation), tested:**
  - Permission mode `default` (or `acceptEdits`) + `permissions.allow:["Bash","Read","Edit","Write"]` + `permissions.ask:["Bash(rm *)","Bash(git reset --hard*)","Bash(ssh *)","Bash(sudo *)","Bash(git push --force*)","Read(./.env)","Read(~/.ssh/**)"]` via `--settings`: README edit with **no** `can_use_tool`; `rm victim.txt` (the model sent `cd "<dir>" && rm victim.txt`, so compound commands are matched per subcommand) **asked**; the `Read` tool on `.env` **asked**. So full auto except a list works with ask rules alone.
  - **`bypassPermissions` ignores `ask` rules**: `rm victim.txt` ran with no request and the file was deleted (scratch repo only). Do not use bypass for the runtime path.
  - `Read(./.env)` does **not** cover `cat .env` through Bash: in a fresh session the model printed the file and nothing was asked. Adding `Bash(*.env*)` and `Bash(*.ssh*)` ask rules made it ask. A `PreToolUse` hook (command printing `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"…"}}` for any Bash input matching `\.env|\.ssh`) also made it ask, and the request still arrived as `can_use_tool`. **Answer: no hook is needed for the starter list; ask rules cover it. Reading credentials through arbitrary shell commands is only best-effort pattern matching (rules or a hook, same strength).** Not verified: `ssh`, `sudo`, `git reset --hard` and force push were not run (only `rm` and `.env`), and the user's real `~/.claude` settings were switched off (`--setting-sources ""`), so interaction with the owner's own allow rules is untested.

### codex (`codex app-server`, native)
JSON-RPC on stdio; frames omit `"jsonrpc":"2.0"`. Trimmed transcript:
```
-> initialize {clientInfo:{name,version}, capabilities:{experimentalApi:true}}   <- result {codexHome, platformOs, userAgent}
-> notification {"method":"initialized"}
-> thread/start {cwd, approvalPolicy:"untrusted", sandbox:"workspace-write", ephemeral:true}   <- result {thread:{id}, approvalPolicy, sandbox:{type:"workspaceWrite",…}}
-> turn/start {threadId, input:[{type:"text",text:"…"}]}
<- thread/started, turn/started {turn:{status:"inProgress"}}, item/started, item/agentMessage/delta, item/completed, thread/tokenUsage/updated, hook/started …
<- REQUEST item/commandExecution/requestApproval {id:0, params:{kind:"command", command:"/bin/zsh -lc 'rm victim.txt'", cwd, itemId, turnId,
     availableDecisions:["accept",{acceptWithExecpolicyAmendment:{…}},"cancel"], proposedExecpolicyAmendment:["rm","victim.txt"]}}
-> {"id":0,"result":{"decision":"decline"}}        <- serverRequest/resolved, item/completed {status:"declined"}
<- turn/completed {turn:{status:"completed", durationMs:9626, items:[{type:"agentMessage",text:"Not done: the shell command was rejec…"}]}}
```
- Turn end = `turn/completed`; its `status` was `completed` also after a decline (same shape as claude: a decline is not a stop reason). Other server requests in the schema: `item/fileChange/requestApproval`, `item/permissions/requestApproval`.
- `decline` is accepted although `availableDecisions` only listed `accept`/`cancel`: the item ended `declined` and `victim.txt` survived. Decision names come from the generated schema (`codex app-server generate-json-schema --experimental`): `accept`, `acceptForSession`, `acceptWithExecpolicyAmendment`, `decline`, `cancel`.
- **Guard config:** `approvalPolicy` is `untrusted | on-request | never` (+ granular object), `sandbox` is `read-only | workspace-write | danger-full-access`. `untrusted` asks for almost every command (the README append asked too), so it is not "full auto except a list". What works: **exec-policy rules** in `<cwd>/.codex/rules/*.rules` (project trusted via `-c 'projects."<dir>".trust_level="trusted"'`): `prefix_rule(pattern=["rm"], decision="prompt", justification="guarded: delete")` with `approvalPolicy:"on-request"` and `sandbox:"danger-full-access"`. The README append ran with no request; `rm victim.txt` asked with `reason: "… requires approval: guarded: delete"` and `availableDecisions:["accept","cancel"]`. Not verified: the same rules from `$CODEX_HOME/rules` (would need an isolated CODEX_HOME with the owner's login, not done), the other starter patterns, reading `.env`/`~/.ssh` (a prefix rule on the executable cannot see file arguments of `cat`; a rule `["cat", ".env"]` would be exact-prefix only), and `turn/interrupt`.
- Not isolated: the child used the owner's real `~/.codex`; its hooks ran (15 `hook/started` frames) and 46 `mcpServer/startupStatus/updated` notifications arrived. A production adapter needs an isolated `CODEX_HOME` as in section 6.

### Codex path: codex-acp vs native `codex app-server`
Research only for codex-acp (nothing installed; the repos' GitHub pages fetched 2026-11-13, so details below are what those pages say):
- `zed-industries/codex-acp` is **archived (read-only since 2026-07-22)**; development moved to `agentclientprotocol/codex-acp`, "built on the new Codex App Server". That repo is **Node/TypeScript** (npm scripts, vitest), run with `npx -y @agentclientprotocol/codex-acp` or a global npm install; it bundles a compatible `@openai/codex` dependency (`CODEX_PATH` overrides it) and can be built into standalone binaries (`npm run bundle:all`). The older Zed one was a Rust binary. Release cadence and latest version: not verified (the releases section was empty in the fetch; release-please plus `@preview` tags on every push to main suggest fast-moving). 81 open issues, 646 commits.
- Permissions: modes `read-only`, `workspace-write`, `agent`, `agent-full-access`, `permission-profile:<id>`; requests surface as ACP `session/request_permission`. Details not verified.
- Cost comparison:

| | codex-acp via `runtime/acp` | native `codex app-server` |
|---|---|---|
| Kernel code | one config entry (command, args, mode map) | a third adapter `runtime/codexappserver` (~JSON-RPC client + mapping; the client loop is the same as `acp`, the method names and approval shapes differ) |
| New dependency on the host | Node + npm package (the sidecar was removed on 2026-11-09 to get rid of Node), or a self-built bundle | none: the `codex` binary already installed |
| Moving parts | ACP adapter wrapping app-server wrapping the model | app-server only |
| Guard | `session/request_permission` + mode map; the guard-rule translation is codex-acp's own mode names (coarse) | exec-policy `prompt` rules in `.codex/rules` + `on-request` + `danger-full-access`, tested; finer than modes |
| Maturity | unverified, young repo that just changed owners | the protocol is marked experimental by Codex (`generate-json-schema --experimental`), pinned by the installed version |

Recommendation: **native `codex app-server` as a third adapter, after phases 2 and 3**, not codex-acp. The adapter would reuse the stdio JSON-RPC client written for `acp`, so the cost is a mapping layer (`item/*` and `turn/*` to the `RuntimeEvent` kinds, `item/commandExecution/requestApproval` to `permission_request`, `Approve` answering with `accept`/`decline`). It avoids a Node dependency and an unverified adapter, and the guard translation was tested here. This contradicts the plan's "start with codex-acp" and open question 3 (decision 1 said two adapters; this makes three), so it needs the owner's decision. If a third adapter is rejected, codex-acp stays the fallback and would need its own spike (Node install, permission modes) that was not done.

### Surprises / for the plan
1. Claude on macOS needs **`USER`** in the child env to find its keychain login (`PATH`+`HOME` alone gives "Not logged in"); the `CLAUDE_CODE_OAUTH_TOKEN` path used by `claudesdk` avoids it. Add `USER` (not a secret) to the child-env list, or document the token path.
2. ACP model/mode switching is `session/set_config_option`, not `session/set_model`/`set_mode`; the port's `Model, Mode` fields should map to config option ids discovered in `session/new`.
3. opencode ends a rejected turn with `stopReason:"cancelled"`; claude and codex continue the turn. `RunDelegated` must not read `cancelled` as a user cancel when it came from our own reject.
4. `bypassPermissions` ignores claude `ask` rules: never the runtime mode.
5. Claude's `Read(.env)` rule does not guard `cat .env`; every runtime's credential-read guard is pattern-matching on commands and is a guard rail, as rule 2 already says.
6. Claude's `system init` frame arrives after the first user message; the `initialize` control response carries `commands` and `models` before it.
7. Not run: `session/load`/resume on any runtime, images, session cancel (`session/cancel`) on opencode, `turn/interrupt` on codex, and long-running multi-turn behaviour.
