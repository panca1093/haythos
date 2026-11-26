# inbox: per-conversation lanes on the event log, with interrupt (plan, 2026-11-26)

## Why

HaythOS moves toward the OpenBot shape: a persistent agent identity with a durable queue in front of it, and runtimes as the execution engines. Today there is no queue. `discord.onMessage` and `ws` each start a goroutine that calls `HandleInput`, which creates a task and runs it to the end (up to `handleTO` = 30 min). The results:

- Two quick messages in one thread run as **two concurrent tasks**. The second one loads conversation memory before the first one writes its `conv.turn`, so it answers without knowing the first exchange.
- There is **no way to stop** a chat turn on the kernel's loop. `CancelDelegated` exists for runtime sessions (ws only); Discord has nothing.
- A message that arrives while the kernel is down or mid-crash is lost. Only the task log survives.

OpenBot solves this with a `MailboxStore` (mutable rows, FIFO + lanes) and a drain scheduler. HaythOS already has an append-only event log with a global `cursor` and streams keyed by `task_id` (`conv:<replyTo>` is already a non-task stream). So the queue is **the log plus a consumer offset**, not a new table.

## Decisions

1. **A lane is one conversation: `replyTo`.** Its inbox is the stream `inbox:<replyTo>` in `events`, following `convStream`. Lanes run in parallel; inside a lane, at most one turn runs at a time. Scheduled jobs (`RunJob`) and delegated tasks keep their own paths and never wait behind a lane.
2. **Events on the inbox stream:**
   - `inbox.msg`: `{text, dm, workdir, model}`, appended by `HandleInput`. `HandleInput` keeps its signature and stays blocking: it waits until an `inbox.ack` covers its message's cursor (or its own ctx ends) and returns that task id. Messages coalesced into one turn all return the same id. Channels, their typing indicator and existing tests keep working unchanged. If the caller's ctx ends first, the message stays durable and the worker still runs it.
   - `inbox.ack`: `{through: <cursor>, task: <id>}`. The lane's offset is the `through` of its last ack, found by `Load("inbox:<replyTo>")`. No offset table.
   - `inbox.interrupt`: `{reason}`, the durable record of a stop (decision 5).
3. **Drain = coalesce everything after the offset into one task.** The worker loads the unacked `inbox.msg` events, joins their texts in order (blank line between), and runs one task. A burst of three messages is one turn that sees all three, which is what a person typing in pieces means.
4. **Crash safety without a cross-stream transaction.** The task id is derived from the last coalesced cursor: `chat-<cursor>`. Order: create task (skipped if `Load(id)` already has events), run it, append `inbox.ack`. A crash before the ack leaves the lane unacked; on restart the same id is found and `RunTask` resumes it from its snapshot. Side effects already in flight are covered by the ledger and `reconcile` (rule 6), as today. At-least-once delivery, effectively-once effects.
5. **Interrupt is out of band, never a queued message.** It must not wait behind the turn it is stopping.
   - The lane keeps `cancel context.CancelCauseFunc` for its running turn. `App.Interrupt(ctx, replyTo)` appends `inbox.interrupt`, then calls `cancel(ErrInterrupted)`.
   - `run` today returns `ctx.Err()` and leaves the task `running`, so it would resume on restart. New: when `context.Cause(ctx)` is `ErrInterrupted`, the task goes `running → failed` with reason `interrupted`, and the channel gets one short notice ("Stopped.").
   - A tool call already `dispatching` is not rolled back. It ends as the ledger says, or `outcome_unknown` for `reconcile`; nothing is retried (rule 6).
   - If the lane's running task is delegated, `Interrupt` calls the existing `CancelDelegated` path, which stops the session (`session/cancel`, grace, kill).
   - **Queued messages after an interrupt are kept and run** *(owner decision, 2026-11-26)*. The worker drains them as the next turn, like any other.
   - **Append, not merge.** The kernel does not merge the stopped objective into the next turn: judging whether "use Postgres" continues or replaces the stopped request needs a model call, and resuming it on its own would undo the stop. Instead the stopped turn is recorded as conversation memory: a `conv.turn` with `Answer: "[stopped by the owner before finishing]"` (no brain marker, so partial work never reaches long-term memory). The next turn sees the stopped request as context, and the model decides from the new messages whether to continue it, change it or drop it. Today `recordTurn` runs only in `finish`, so a failed or stopped task is invisible to the next turn.
   - Entry points: Discord text command `/stop` (added to `textCommand`), a Stop button on the typing/progress notice if cheap, and a ws frame next to the existing cancel frame.
6. **`waiting_approval` and `watching` release the lane.** The worker returns when the task leaves `running`, and acks the messages it consumed. Otherwise one unanswered approval blocks the conversation forever. New messages start a new task as they do today; the approval buttons still work on the old one.
7. **Worker lifecycle: one goroutine per busy lane, exits when the lane is empty.** `lanes map[string]*lane` behind a mutex; `HandleInput` appends, then starts the worker if none runs (`wake chan struct{}` with buffer 1, so a message that lands mid-drain is picked up by the next loop). No idle pool, no global concurrency cap (single user). On `Start`, every inbox stream with an `inbox.msg` after its last ack gets a worker.
8. **Priority is lanes + interrupt, not a priority queue.** Between conversations: parallel lanes, so a long chat in one thread never blocks another or a cron job. Inside one conversation: FIFO plus interrupt. A real priority need (for example, the owner's DM jumping ahead of an agent-to-agent message once there are several agents) gets its own decision when it exists.

## Out of scope

- **Steer** (inject a new message into the running turn between steps). The loop rereads the snapshot every step, so it is cheap later: append the message to the running task instead of the inbox. It changes what "one turn" means, so it waits for a real need.
- Several agents, agent-to-agent `send_message`, channels with a lead (OpenBot's multi-agent layer). The lane key becomes `<agent>:<replyTo>` then.
- Retention or compaction of inbox streams.

## Steps

1. `app/inbox.go`: event types, `lane`, worker loop, coalescing, deterministic task id, ack. **Verify:** test with `testfakes` store: three `HandleInput` calls on one `replyTo` while the model fake blocks give exactly two tasks (the first message, then the other two coalesced), in order; two different `replyTo`s run concurrently.
2. Crash resume. **Verify:** in-process crash test on a temp SQLite file, stopped by the existing `a.hook` at a new point `inbox.before_ack`; a new `App` on the same database calls `Start`: the same task finishes and one ack is written, no second task.
3. Interrupt on the kernel loop. **Verify:** test with a blocking model fake and two messages queued behind the turn: `Interrupt` makes the task `failed` with reason `interrupted`; the two queued messages then run as one coalesced turn whose history contains the stopped request as a context `conv.turn` with the stopped marker; no brain marker for the stopped task. A write tool mid-dispatch keeps its ledger state.
4. Interrupt on a delegated task through `CancelDelegated`. **Verify:** existing runtime fake: session `Stop` called, task failed as cancelled.
5. Approval releases the lane. **Verify:** test: task hits `waiting_approval`, a second message in the lane starts a new task without waiting.
6. Channels: Discord `/stop` text command and a ws interrupt frame, both calling `App.Interrupt(ctx, replyTo)`. **Verify:** channel tests for `/stop` routing; manual run in a Discord thread: send a long request, `/stop`, then a new message.
7. `go test ./...` and `go vet ./...` green; update CLAUDE.md ports/flow section.

## Open questions

1. ~~Drop or keep queued messages on `/stop`?~~ Keep and run, appended (decision 5).
2. ~~Typing indicator.~~ Unchanged, since `HandleInput` stays blocking.
3. **`handleTO`.** The worker runs on its own context (from `Start`), bounded by `StepTimeout` per step and the job budget; the channel's 30-min ctx only bounds how long the caller waits.
4. `// TODO: uncertain` — `conversation()` reads `conv.turn` to build `prior`. With serialized lanes the race disappears, but verify that `conv.turn` is written before the task's final notice, so the next drained turn always sees it.
