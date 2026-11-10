# One `haythos` binary, single WebSocket client API, TUI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Chat with Alfred in a terminal UI while Discord and the scheduler stay live, with the kernel running as a daemon behind exactly one WebSocket door.

**Architecture:** The kernel (`haythos serve`) keeps Discord as an in-process adapter and adds `adapters/channel/ws`, one WebSocket server on a unix socket for all of our own UIs. A routing notifier in `cmd/haythos` sends each `app.Notice` to the adapter that owns its `ReplyTo` (`tui:` → ws, anything else → Discord as today). `haythos chat` is a client: it starts the daemon if needed, opens a new `tui:<id>` conversation and renders it with Bubble Tea.

**Tech Stack:** Go 1.27, `github.com/coder/websocket` (stdlib has no WebSocket server), `github.com/charmbracelet/bubbletea` + `github.com/charmbracelet/lipgloss` (terminal UI), SQLite event log (existing).

**Spec:** decisions recorded with the owner on 2026-11-10 (copied into Global Constraints below; the design spec itself is private). Read `CLAUDE.md` first.

## Global Constraints

- One binary `haythos` (rename of `cmd/haythosd`). Subcommands: `serve`, `chat`, `sessions`, `status`, `stop`, `service install|uninstall`, plus existing `run-job`, `auth`, `setup`.
- Exactly one WebSocket endpoint for every interactive client. Never one WS per channel. Discord stays an in-process adapter; no bridge process.
- Socket: `<home>/run/haythos.sock`, dir mode 0700, socket mode 0600, where `<home>` is `$HAYTHOS_HOME` or `~/.haythos`. No TCP listener in this change. Remote access = SSH unix-socket forwarding, documented in README only.
- Each `haythos chat` starts a NEW conversation `tui:<12 hex chars>`; `--resume` continues the latest TUI conversation, `--session <id>` a given one.
- WS clients may only act on `tui:` conversations. An input or approval for any other origin is refused.
- Approvals appear only at their origin conversation and may only be decided from it. Never cross-posted.
- Quitting the TUI never stops the kernel. No idle-exit.
- `HAYTHOS_VAULT_KEY` wins when set; otherwise the key comes from the OS keychain. The key is never printed, logged, or written into a plist/unit file.
- Discord `ReplyTo` values stay as they are today (raw channel/thread ids); no data migration. New origins use a `<client>:` prefix.
- Do not edit `tests/acceptance/`. `make gate` green at every commit. Commits `<area>: <what>`, authored as the repo owner, no co-author or tool attribution.

## Review Focus

- Stale socket file left by a crashed kernel → `serve` removes it when no one answers on it and starts; `chat` does not hang on it.
- Two `haythos chat` started at the same moment with no kernel → exactly one kernel is spawned (spawn lock), both attach.
- A WS client sends `input` or `approval.decide` for a Discord origin or another TUI conversation's task → refused with an `error` frame, nothing runs.
- TUI closed while its task waits for approval → task stays `waiting_approval`; reattaching with `--resume` shows the approval again and deciding it resumes the task.
- Client and kernel versions differ (owner rebuilt the binary, old daemon still running) → TUI shows a one-line warning naming `haythos stop` and still works.

---

### Task 1: Rename the binary to `haythos`

**Files:**
- Move: `cmd/haythosd/` → `cmd/haythos/` (`git mv`, keep history)
- Modify: `Makefile`, `README.md`, `.gitignore` (`/haythosd` → `/haythos`), `CLAUDE.md` layout line `cmd/haythosd/` → `cmd/haythos/`, user-facing strings that say `haythosd` (usage text, hints such as "run `haythosd auth`")

**Interfaces:**
- Produces: package `main` at `cmd/haythos`; every later task adds files there.

- [ ] **Step 1:** `git mv cmd/haythosd cmd/haythos`; update strings and docs; `grep -rn haythosd --include='*.go' --include='*.md' --include=Makefile .` returns only historical notes in `docs/REPORT.md` and `docs/plans/`.
- [ ] **Step 2:** `make gate` → PASS.
- [ ] **Step 3: Commit** `cmd: rename haythosd to haythos`

### Task 2: Conversation read API in `app`

**Files:**
- Create: `app/convview.go`
- Test: `app/convview_test.go`

**Interfaces:**
- Produces:
  - `type TurnView struct { User, Answer string; At time.Time }`
  - `type PendingApproval struct { TaskID string; Approval ApprovalRequest }`
  - `type ConversationView struct { Turns []TurnView; Pending []PendingApproval }`
  - `type ConversationSummary struct { ReplyTo, FirstUser string; LastAt time.Time }`
  - `func (a *App) ConversationView(ctx context.Context, replyTo string) (ConversationView, error)` — turns from `convStream(replyTo)` (`conv.turn` events, oldest first); pending = tasks whose `task.Created.ReplyTo == replyTo` and state `waiting_approval`, with the last `approval.requested` payload.
  - `func (a *App) Conversations(ctx context.Context, prefix string) ([]ConversationSummary, error)` — conversations whose replyTo starts with `prefix`, newest `LastAt` first.
  - `func (a *App) TaskOrigin(ctx context.Context, taskID string) (string, error)` — `Created.ReplyTo`.

- [ ] **Step 1: Failing tests** (fake event store from `adapters/testfakes`): `TestConversationViewTurnsAndPending` (two turns on `tui:a`, one waiting task on `tui:a`, one waiting task on `tui:b` → view of `tui:a` has 2 turns and exactly 1 pending with its approval id); `TestConversationsByPrefix` (`tui:a`, `tui:b`, and a Discord id `123` → prefix `tui:` returns 2, newest first, `FirstUser` = first turn's user text truncated to 60 runes); `TestTaskOrigin`.
- [ ] **Step 2:** `go test ./app -run 'ConversationView|Conversations|TaskOrigin'` → FAIL.
- [ ] **Step 3: Implement** by scanning `Events.Since(0)` once per call. Add `// TODO: uncertain — full event-log scan per call, fine for one user; add an index if the log grows past ~1e5 events`.
- [ ] **Step 4:** PASS with `-race`.
- [ ] **Step 5: Commit** `app: read conversations, pending approvals and task origin`

### Task 3: WebSocket protocol and server

**Files:**
- Create: `adapters/channel/ws/protocol.go`, `adapters/channel/ws/server.go`
- Test: `adapters/channel/ws/server_test.go`

**Interfaces:**
- Consumes: Task 2 methods.
- Produces:
  - Frames: one JSON object per text frame, field `type`.
    - Client → server: `hello{client, instance, version}`, `subscribe{conv}`, `input{conv, text}`, `approval.decide{task_id, approval_id, approve}`, `sessions{}`.
    - Server → client: `welcome{version, pid, started_at, channels}`, `history{conv, turns, pending}`, `notice{kind, task_id, text, state, reason, approval}` (`kind` = `app.NoticeKind` value), `sessions.list{items}`, `error{message}`.
    - `approval` in frames = `{id, tool, target, tier, preview, escalated}` from `app.ApprovalRequest`; never tool args beyond the preview.
  - `type Handler interface { HandleInput(ctx, app.Input) (string, error); HandleApproval(ctx, taskID, approvalID string, approve bool) error; ConversationView(ctx, replyTo string) (app.ConversationView, error); Conversations(ctx, prefix string) ([]app.ConversationSummary, error); TaskOrigin(ctx, taskID string) (string, error) }`
  - `type Config struct { SocketPath, Version string; StartedAt time.Time; Channels []string }`
  - `func New(cfg Config, h Handler) *Server`; `func (s *Server) Serve(ctx context.Context) error` (blocks until ctx done; removes the socket on exit); `func (s *Server) Publish(ctx context.Context, n app.Notice) error` (implements `app.Notifier`; sends to every connection subscribed to `n.ReplyTo`; no subscriber is not an error).
  - `const Prefix = "tui:"`.

- [ ] **Step 1: Failing tests** (real unix socket in `t.TempDir()`, fake Handler, client written with `coder/websocket` in the test):
  - `TestHelloWelcome`: welcome carries config version and channels.
  - `TestSubscribeSendsHistory`: history frame equals the handler's view for that conv.
  - `TestInputRoutesWithOrigin`: `input{conv:"tui:x"}` → handler got `app.Input{Text, ReplyTo:"tui:x"}`.
  - `TestRejectsForeignOrigin`: `input{conv:"123"}` and `approval.decide` for a task whose origin is `tui:other` → `error` frames, handler not called.
  - `TestPublishOnlyToSubscribers`: clients on `tui:a` and `tui:b`; `Publish(Notice{ReplyTo:"tui:a"})` reaches only the first.
  - `TestSocketPermissions`: socket mode `0600`, parent dir `0700`.
  - `TestStaleSocketReplaced`: a leftover socket file with no listener is removed and `Serve` starts; a live listener on the path makes `Serve` return an error containing "already running".
- [ ] **Step 2:** `go test ./adapters/channel/ws` → FAIL.
- [ ] **Step 3: Implement.** Add the dependency (`go get github.com/coder/websocket`). Serve with `net.Listen("unix", …)` + `http.Server`; one goroutine per connection; per-connection write mutex; `subscribe` replaces the connection's conversation (one conv per connection). Approval decide checks `TaskOrigin == connection conv` before calling the handler. Publish drops a frame for a connection whose write fails and closes it.
- [ ] **Step 4:** PASS with `-race`; `golangci-lint run ./adapters/channel/ws/...` clean.
- [ ] **Step 5: Commit** `ws: one WebSocket client API on a unix socket`

### Task 4: WS client library

**Files:**
- Create: `adapters/channel/ws/client.go`
- Test: `adapters/channel/ws/client_test.go`

**Interfaces:**
- Produces: `func Dial(ctx context.Context, socketPath string) (*Client, error)`; `func (c *Client) Hello(ctx context.Context, client, instance, version string) (Welcome, error)`; `func (c *Client) Send(ctx context.Context, frame any) error`; `func (c *Client) Next(ctx context.Context) (Frame, error)` where `Frame` is the decoded server frame (`Type` + typed fields from protocol.go); `func (c *Client) Close() error`.

- [ ] **Step 1: Failing test** `TestClientRoundTrip` against a `Server` from Task 3 with a fake handler: hello → subscribe → input → receives a published notice.
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS.
- [ ] **Step 5: Commit** `ws: client for the kernel socket`

### Task 5: Serve the socket, route notices, `status` and `stop`

**Files:**
- Create: `cmd/haythos/home.go` (paths), `cmd/haythos/route.go` (routing notifier), `cmd/haythos/ctl.go` (`status`, `stop`)
- Modify: `cmd/haythos/main.go` (start ws server in `serve`, write `<home>/run/haythos.pid`, remove it on exit)
- Test: `cmd/haythos/route_test.go`, `cmd/haythos/kernel_ws_test.go`

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces:
  - `type paths struct { Home, Run, Socket, PID, SpawnLock, Logs string }`; `func homePaths(getenv func(string) string) (paths, error)`.
  - `type routeNotifier struct { ws, other app.Notifier }` — `ReplyTo` with prefix `ws.Prefix` → `ws`, everything else (including empty) → `other` (Discord or stdout, exactly as today).
  - `status` prints `running pid <n> since <RFC3339> version <v> channels <list>` or `not running`; `stop` sends SIGTERM to the pid from the pid file and waits up to 10 s for the socket to go away.

- [ ] **Step 1: Failing tests:**
  - `TestRouteNotifier` table: `tui:x` → ws only; `123456` → other only; `""` → other only.
  - `TestKernelOverWS` (in-process `app.App` with testfakes model scripted to request one tier-1 write tool, real sqlite in `t.TempDir()`, real ws server): client A on `tui:a` sends input → receives `approval.request`; client B on `tui:b` receives nothing; B deciding A's approval gets `error`; A disconnects; a new client subscribes to `tui:a` → `history` lists the pending approval; deciding it → `message.done` arrives.
  - `TestStatusNotRunning` with an empty home.
- [ ] **Step 2:** FAIL. **Step 3:** implement; `serve` logs one line `listening` with the socket path at Info. **Step 4:** PASS with `-race`.
- [ ] **Step 5: Commit** `cmd: serve the client socket, route notices by origin, add status and stop`

### Task 6: `chat` starts the daemon on demand

**Files:**
- Create: `cmd/haythos/daemon.go`
- Test: `cmd/haythos/daemon_test.go`

**Interfaces:**
- Produces: `func ensureKernel(ctx context.Context, p paths, spawn func() error, dial func(context.Context) error) error` — dial; on failure take an exclusive `flock` on `p.SpawnLock`, dial again, else `spawn()` and poll `dial` every 200 ms for up to 15 s. Real `spawn` = `exec.Command(os.Executable(), "serve")` with `SysProcAttr{Setsid: true}`, stdin `/dev/null`, stdout+stderr appended to `<home>/logs/haythos.log`, environment inherited (carries `HAYTHOS_VAULT_KEY` when set).

- [ ] **Step 1: Failing tests:** `TestEnsureKernelAlreadyUp` (spawn never called); `TestEnsureKernelSpawnsOnce` (8 concurrent callers sharing a fake whose dial succeeds after the first spawn → spawn called exactly once); `TestEnsureKernelTimeout` (dial never succeeds → error mentions the log path).
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS with `-race`.
- [ ] **Step 5: Commit** `cmd: start the kernel in the background when a client needs it`

### Task 7: Vault key from the OS keychain

**Files:**
- Create: `adapters/vault/keychain/keychain.go`, `adapters/vault/keychain/keychain_test.go`
- Modify: `adapters/vault/file/vault.go` (`KeyFromEnv` gains a fallback), `cmd/haythos/main.go`

**Interfaces:**
- Produces: `type Keychain struct{ run func(ctx context.Context, name string, args ...string) ([]byte, error) }`; `func New() *Keychain`; `func (k *Keychain) Get(ctx context.Context) (string, error)`; `func (k *Keychain) Set(ctx context.Context, value string) error`; `var ErrNotFound`. Service `haythos`, account `vault-key`. macOS: `security find-generic-password -s haythos -a vault-key -w` / `security add-generic-password -U -s haythos -a vault-key -w <value>`. Linux: `secret-tool lookup service haythos account vault-key` / `secret-tool store --label=haythos service haythos account vault-key` (value on stdin). Other OS → `ErrNotFound`.
- `func file.Key(ctx context.Context, getenv func(string) string, kc interface{ Get(context.Context) (string, error) }) ([]byte, error)` — env wins; else keychain; same base64/32-byte validation as today.

- [ ] **Step 1: Failing tests** with a fake `run`: argv per OS for Get/Set, `ErrNotFound` on exit status 44 (macOS) / 1 (Linux) with empty output, value never appears in returned errors; `TestKeyEnvWins`, `TestKeyFromKeychain`, `TestKeyMissingBoth` (error names both sources).
- [ ] **Step 2:** FAIL. **Step 3:** implement; macOS `add-generic-password -w` puts the key in argv for a moment — add `// TODO: uncertain — key briefly visible in the process list during service install, verify acceptable` if no stdin form exists. **Step 4:** PASS.
- [ ] **Step 5: Commit** `vault: read the key from the OS keychain when the env var is unset`

### Task 8: `service install` / `uninstall`

**Files:**
- Create: `adapters/service/service.go`, `adapters/service/service_test.go`, `adapters/service/testdata/haythos.plist.golden`, `adapters/service/testdata/haythos.service.golden`
- Modify: `cmd/haythos/main.go`

**Interfaces:**
- Produces: `func LaunchdPlist(label, execPath, logPath string) []byte` (label `io.github.panca1093.haythos`, ProgramArguments `[execPath, "serve"]`, RunAtLoad, KeepAlive, StandardOut/ErrorPath = logPath, no EnvironmentVariables); `func SystemdUnit(execPath, logPath string) []byte` (user unit, `Restart=on-failure`); `type Installer struct { Home, ExecPath string; GOOS string; Run func(ctx, name string, args ...string) error; WriteFile func(path string, b []byte, mode os.FileMode) error }` with `Install(ctx) error` / `Uninstall(ctx) error` (macOS: write `~/Library/LaunchAgents/<label>.plist`, `launchctl bootstrap gui/<uid> <plist>`; Linux: `~/.config/systemd/user/haythos.service`, `systemctl --user daemon-reload`, `enable --now haythos`).
- `service install` in `cmd`: if the keychain has no key and `HAYTHOS_VAULT_KEY` is set, ask "Store the vault key in the OS keychain so the service can start without it? [y/N]"; on y call `Keychain.Set`; on N abort with a hint. Stops a running non-service kernel first (`stop`) so the service owns the socket.

- [ ] **Step 1: Failing tests:** golden files for both generators (no key text, no `HAYTHOS_VAULT_KEY` anywhere); Installer argv per OS with fakes; uninstall reverses it.
- [ ] **Step 2:** FAIL. **Step 3:** implement. **Step 4:** PASS.
- [ ] **Step 5: Commit** `service: install the kernel as a launchd or systemd user service`

### Task 9: The TUI and `chat` / `sessions`

**Files:**
- Create: `adapters/tui/tui.go`, `adapters/tui/tui_test.go`
- Modify: `cmd/haythos/main.go` (`chat`, `sessions`)

**Interfaces:**
- Consumes: `ws.Client` (Task 4), `ensureKernel` (Task 6).
- Produces: `func New(c Conn, conv string, version string) tea.Model` where `type Conn interface { Send(ctx context.Context, frame any) error; Next(ctx context.Context) (ws.Frame, error) }`. Layout: scrollback (history then live messages), one-line status (`kernel <version> · <conv> · <task state>`), input box. A pending approval renders inline as a block with tool, target, preview and `[y] approve  [n] reject`; y/n sends `approval.decide`. Enter sends `input`. `/quit` and Ctrl+C quit the TUI only. Version mismatch shows one warning line: `kernel <v1> differs from this binary <v2>; run "haythos stop" and start chat again`.
- `chat` flags: none → new conv `tui:` + 12 random hex; `--resume` → newest from `Conversations("tui:")`; `--session <id>` → that conv (error if unknown). `sessions` prints `<id>  <last active>  <first line>` newest first.

- [ ] **Step 1: Failing tests** driving the model with messages (no real terminal): history renders; Enter sends `input{conv,text}`; an approval notice renders the block and `y` sends `approval.decide` with the right ids; version mismatch warning; `/quit` returns `tea.Quit` and sends nothing.
- [ ] **Step 2:** FAIL. **Step 3:** implement (`go get github.com/charmbracelet/bubbletea github.com/charmbracelet/lipgloss`). **Step 4:** PASS; `make gate` green.
- [ ] **Step 5: Commit** `tui: chat with Alfred over the kernel socket`

### Task 10: Docs and PR

**Files:**
- Modify: `README.md` (install, `haythos chat`, `--resume`, `sessions`, `status`/`stop`, `service install`, SSH forwarding example `ssh -L /tmp/haythos.sock:/home/<user>/.haythos/run/haythos.sock vps` with `HAYTHOS_HOME`-based client override, layout table rows for `adapters/channel/ws`, `adapters/tui`, `adapters/service`, `adapters/vault/keychain`), `docs/REPORT.md` (new section "TUI and client socket" with decisions, uncertain items, "Needs you live").
- [ ] **Step 1:** write docs; "Needs you live": launchd install on the owner's Mac and login restart; keychain prompt on first `security` access; TUI on a real terminal at 80 columns; Discord and TUI chats at the same time with approvals staying at their origin; `--resume` after closing the TUI mid-approval; SSH socket forwarding to a VPS.
- [ ] **Step 2:** `make gate` green; `git diff main -- tests/acceptance` empty.
- [ ] **Step 3: Commit** `docs: TUI, client socket and service`
- [ ] **Step 4:** push `feat/tui-ws-gateway`, open the PR (what/why, the two new dependencies with reasons, verification, uncertain items, needs-live list). Do not merge.
