# TUI redesign + read-only folder access — Plan

Approved with the owner on 2026-11-10 from browser mockups (layout "A").

**Goal:** `haythos chat` looks like a tidy coding-agent terminal: an opening card, the conversation flowing inline, a framed input and a hint line; leaving prints a session summary. Alfred can read files in the folder the chat was opened in.

## Decisions

- Opening card: `◆ Alfred v<short commit>`, `<model> · <default model name>`, the chat folder (home shortened to `~`). No kernel info. Never the word "dirty" on screen; the version-mismatch check keeps the full build id internally.
- Inline (no alternate screen): finished lines go to the terminal scrollback with `tea.Println`; the live area is only the pending approval, the framed input and the hint line.
- Conversation lines: `› <user text>` (blue), `● <Alfred answer>` (accent), tool activity `  ⎿ <tool> <target>` (dim), errors in red.
- Approval block: left bar `┃ Butuh izin · tier N`, `tool → target`, preview, `[y] izinkan  [n] tolak`; the input is locked meanwhile (as today).
- Hint line: `● <state> · exit / /exit / /quit untuk keluar`.
- `exit`, `/exit`, `/quit`, Ctrl+C all quit the TUI only.
- Exit summary (stdout, after the TUI): resume commands (`haythos chat --session <id>`, `haythos chat --resume`), `Sesi`, `Judul` (first user message, ≤ 50 runes), `Durasi` (this run), `Pesan` (`N (U kamu, T tool)` for the whole conversation), `Menunggu` line only when approvals are pending, and a line saying Alfred keeps running in the background. UI copy in Indonesian.
- Folder: the TUI sends its working directory on `subscribe`; the kernel stores it with the TUI input (`app.Input.Workdir` → `task.Created.Workdir`) and passes it to tools through the context. Discord-originated tasks have no workdir.
- New tools `fs.list` and `fs.read`, tier 0, `Untrusted` (reading a file taints the task, rule 4). Paths are relative to the workdir; after resolving symlinks they must stay inside it. Refused: anything under `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.haythos`, `~/.config/gh`, `~/.netrc`, files named `.env*`, `*.pem`, `*.key`, `id_*`, the vault file, and non-UTF-8 files. `fs.read` returns at most 64 KiB (says when truncated); `fs.list` at most 200 entries.
- Tool activity notices: the app publishes `tool.started` (tool name + a short target, never write-tool arguments) to the task's origin; Discord ignores this kind.

## Tasks (TDD, one commit each)

1. `app`: `Input.Workdir`, `Created.Workdir`, `WorkdirFrom(ctx)`, `NoticeTool` published before each tool call; Discord drops `NoticeTool`.
2. `adapters/tools/local/fs`: `fs.list`, `fs.read` with the guards above; policy tiers; default chat job gets both tools.
3. `ws`: `subscribe{conv, workdir}`; inputs carry the connection's workdir; `welcome` gains `model` and `default_model`; `history` gains `user_turns` and `tool_calls` counts for the summary; app gains `ConversationStats`.
4. `cmd/haythos`: `displayVersion()` (short commit only); exit summary printer.
5. `adapters/tui`: new layout, inline rendering, exit words, summary data handed back to `chat`.
6. Docs + PR.
