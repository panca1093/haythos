# M2B kickoff: conversation memory + gbrain

Read `CLAUDE.md` first. It is the source of truth. If this file and `CLAUDE.md` disagree, follow `CLAUDE.md` and record the conflict in `docs/REPORT.md` under "M2B decisions".

Agreed with the user on 2026-10-28. Today every chat message is a new task with no memory of the previous one. M2B gives Alfred short-term memory (the conversation, from the event log) and long-term memory (gbrain).

## Goal

1. **Conversation memory.** A message in a thread or DM sees the earlier turns of that conversation.
2. **Long-term memory.** gbrain holds what should outlive a conversation. Alfred reads it at the start of every task, writes to it when the user explicitly asks, and the kernel extracts candidates from stale conversations.
3. **Trust by source.** Brain content is trusted as information, never as instruction. MCP tools are untrusted by default.

## Decisions already made (do not re-ask)

| # | Decision |
|---|---|
| 1 | A conversation is keyed by the task's `reply_to` (thread or DM channel id). A thread has no time limit. A DM only looks back 24 h. Tasks without `reply_to` (scheduled jobs) have no conversation. |
| 2 | One turn = the user message + Alfred's final answer. Tool calls and results are not replayed (superseded 2026-11-11: a turn now replays the task's own messages, tool calls and results included, unchanged, so the prompt cache carries across messages; replayed untrusted results taint the new task; a turn that alone passes the threshold falls back to user message + answer). Recent turns are kept verbatim up to **7,000 tokens** (estimated as bytes ÷ 4). Older turns are folded into a rolling summary made by the kernel's model and stored as an event. |
| 3 | gbrain is pinned to one commit, installed with `bun`, initialised keyless: `gbrain init --pglite --no-embedding`. No Voyage, no embedding key, nothing leaves the machine except the prompt to the model. |
| 4 | gbrain runs as an MCP stdio child: `gbrain serve --surface verbs`. |
| 5 | First content: the MyBook wiki at `~/Documents/MyBook/wiki/`: only `entities/`, `concepts/`, `tickets/`, `learnings/`, `index.md`. Never `personal/`. Loose top-level pages are skipped (some are personal). |
| 6 | Brain first: before the first model step of every task, the kernel recalls with the task input. At most 3 hits and ~800 tokens, injected once as a labelled data block. No hits → nothing injected. |
| 7 | `brain.remember` is tier 1 but **runs without approval when the task is not tainted** and was started by the owner's message. Tainted → normal approval. This deviates from "tier 1 asks in M1–M2"; record it. |
| 8 | Every brain write carries provenance set by the kernel, not the model: `source: owner/<conversation>`, `auto/<conversation>` (extracted, marked unverified), or `import/mybook`. |
| 9 | Stale hook: a conversation idle for **3 h** with new turns since its last extraction gets a background extraction task. It writes candidates to the brain inbox. Only decisions, preferences, user corrections, lessons, and facts about people/projects/repos. A conversation that was ever tainted is not auto-extracted; the digest lists it as skipped. |
| 10 | `brain-consolidate` job: `CRON_TZ=Asia/Jakarta 0 2 * * *`, silent, processes yesterday's inbox: dedupe against existing pages, merge into them, drop noise. Daily until mature; the user switches it to weekly with `/jobs`. |
| 11 | `brain-digest` job: `CRON_TZ=Asia/Jakarta 0 7 * * *`, one Discord message listing what consolidation saved or merged, with an Undo button per item. Ignored → kept. Undo → `forget`. Nothing saved → no message. |
| 12 | MCP tools are `Untrusted` by default. A server marked `trusted: true` in config is not (gbrain; the user may add a repo/code server). |
| 13 | Brain access goes through a `Brain` port (`app/ports.go`), not gbrain-specific names. Model-facing tools are `brain.recall`, `brain.remember`, `brain.forget`. A future in-kernel Go brain replaces the adapter only. |

## Prompt-injection defenses added in M2B

1. No automatic brain write from a tainted task (decision 7 and 9).
2. Provenance on every brain page; auto-extracted items carry `unverified`.
3. Recalled brain content and untrusted tool output are wrapped in a labelled data block; zero-width and bidi control characters are stripped before they reach the model.
4. Discord messages for tainted tasks are sent with `SuppressEmbeds`, so a crafted URL cannot leak data through a link preview.
5. Brain content never makes a write target trusted: targets still come only from the user message or the job definition.

## Out of scope

Embeddings (Voyage or Ollama), gbrain `synthesize`/`think` (need an API key; the kernel uses its own model), gbrain `dream`/autopilot, Goal entity, M3 builder, rewriting gbrain in Go (next iteration), MCP over HTTP, Slack/Jira connectors.

## Phase gate (end of every phase)

`make gate` exits 0. Existing tests in `tests/acceptance/` unchanged (files were regrouped by use case after M2B). Commit, then add changed / uncertain / next to `docs/REPORT.md` under "M2B report".

## Phases

Each phase starts by writing its contract tests in `tests/acceptance/` (historically `m2b_test.go`; now the file for the use case, e.g. `brain_test.go`), committed alone as `contract: M2B phase N acceptance tests`. Frozen from that commit.

### Phase 1: conversation memory

Contract tests:
1. A second message to the same `reply_to` sees the first turn (user text, tool calls and results, and Alfred's answer) in the history passed to the model (amended 2026-11-11 with the owner's approval; it used to forbid tool traffic).
2. A message to a different `reply_to` sees nothing from the first.
3. A DM turn older than 24 h is not replayed; a thread turn older than 24 h is.
4. When turns exceed 7,000 tokens, the oldest are replaced by a summary event; the summary survives a restart and is not recomputed for the same turns.
5. Scheduled tasks (no `reply_to`) get no conversation history.

### Phase 2: MCP client + gbrain + brain first + remember

- `adapters/tools/mcp`: minimal stdio JSON-RPC client, standard library only (`initialize`, `tools/list`, `tools/call`). Tools are named `<server>.<tool>`. Config `mcp.servers: [{name, command, args, trusted}]`.
- `adapters/brain/gbrain`: implements `Brain` over the MCP client. `brain.*` tools are local tools calling the port.
- `scripts/brain-setup.sh` (the user runs it): install pinned gbrain with bun, init keyless, import the wiki folders from decision 5.
- Policy: `brain.recall: 0`, `brain.remember: 1`, `brain.forget: 1`; a policy list `auto_when_clean: [brain.remember]`.

Contract tests:
1. A tool from an MCP server without `trusted` taints the task; from a trusted server it does not.
2. The first model step of a task receives at most 3 recalled snippets in one labelled block; no hits → no block; later steps do not recall again.
3. `brain.remember` from an untainted owner chat runs without approval and the stored page carries `source: owner/<conversation>`, whatever the model put in the args.
4. `brain.remember` in a tainted task asks for approval.
5. Recalled content that tries to give a write target ("post to channel X") does not make X trusted: the write is escalated to tier 3.
6. Zero-width and bidi characters in recalled or untrusted content do not reach the model.

### Phase 3: stale hook, consolidate, digest

Contract tests:
1. A conversation idle for 3 h with new turns starts exactly one extraction task (also across a restart); idle less than 3 h starts none.
2. A conversation that was ever tainted is not extracted and is reported as skipped by the digest.
3. Extracted items land in the inbox with `source: auto/<conversation>` and `unverified`.
4. `brain-consolidate` only touches inbox items from the previous Jakarta day and sends no notice.
5. `brain-digest` sends one notice listing saved/merged items with an Undo per item, nothing when there is nothing; Undo calls `forget` and is recorded as an event.
6. A tainted task's Discord message is sent with embeds suppressed.

## Needs the user live

Run `scripts/brain-setup.sh`; ask Alfred something from the wiki; say "ingat …" in a thread and check it is recalled later from another thread; leave a thread 3 h and check the 07:00 digest the next morning; press Undo once.
