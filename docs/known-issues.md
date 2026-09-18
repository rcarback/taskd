# Known issues

Open items carried out of the daemon and protocol work. Each one is recorded
here because a review found it, decided not to fix it on that branch, and gave
a reason.

## The task_search response size is not bounded

`task_read` clamps its result against `proto.MaxMessageBytes`. `task_search`
does not. A single long matched line can produce a response the protocol
cannot carry, and the client sees a decode failure rather than a useful
error.

The fix is not mechanical. Bounding the response means truncating a matched
line or a context line, which decides whether a truncated line is still a
match and how the client learns that the line was cut. That is an interface
question for the plan owner.

## The `Record.PID` field is declared and never used

`record.Record` carries a `PID` field. Nothing writes it and nothing reads
it. Setting it needs an accessor on `supervisor.Task` and a setter on
`daemon.Entry`, because `start` holds the record by value and the entry keeps
its own copy. Deleting it instead discards the value an operator wants when a
task has to be found by hand. Either direction is a decision, not a cleanup.

## task_write cannot reach a task started with pty false

A task started with `pty: false` gets no input channel, so `task_write`
against it fails. This is deliberate. The alternative was to give every such
task a pipe that nothing closes, which made any task that reads standard
input block forever.

The default path uses a pseudo-terminal, so `task_write` works normally. What
is missing is a client-facing explanation: the failure should say that the
task was started without a pseudo-terminal, rather than describing the
absence in terms of internal types.

## Durability and cleanup

- `record.Save` leaves a temporary file behind if the process dies between
  the temporary write and the rename.
- `record.Save` does not sync the parent directory after the rename, so the
  rename does not survive a power loss.
- `taskdir.New` leaves its directory behind when a later step fails.
- The daemon log opens in append mode without truncation, so it grows across
  repeated startup failures against one root.
- `record.Record` does not persist the task's output cap, so a log reopened
  for reading cannot recover the real limit. This is inert today, because a
  reopened log never appends.

## Durations from a lost record are wrong

After a crash, `reconcile` stamps `EndedAt` with the restart time, because
nothing can recover the true end time. Any duration computed from a record in
the `lost` state is wrong. The documentation needs to say so somewhere a user
reads.

## `watch.ActionNotify` wakes nobody

`watch.CompilePatterns` accepts `watch.ActionNotify` on a start-time pattern.
Once compiled, `ActionNotify` behaves exactly like `watch.ActionRecord`: it
counts the match and records the last line and its capture groups. It wakes
no one. A caller can set `on_match: "notify"` today and get silence, with no
error saying so.

Wiring `ActionNotify` needs broadcast-capable waiter machinery inside `Tap`,
repeatable, observed by every current subscriber. `Tap.Killed()` is
deliberately single-shot, built for a one-time kill signal, and cannot serve
this case. This belongs with the wake adapters of the next plan.

## `deliver: "notify"` degrades to a long poll

No notification delivery path exists yet, so `task_wait` blocks under every
`deliver` value and returns a warning saying so.

## The long-poll warning does not reach standard error or name the harness

The design says the warning `longPollWarning` builds also goes to standard
error for the operator, and names the harness in the text, `harness: codex`.
Neither happens. The warning reaches only the tool result that answers the
call, and it never mentions `StartParams.Harness`.

## `task_wait`'s `until` cannot express a match condition

The MCP adapter's `task_wait` takes `until` as the comma-separated string
`taskd wait --until` accepts, not the object form `watch.Condition` carries
over the socket. `watch.ParseUntil` rejects `match:REGEX` on that string
deliberately, because a regular expression may contain a comma and this
spelling splits on commas. Waking on matched output is therefore
unavailable through `task_wait`, even though the daemon supports it through
the socket API's object form.

`skills/task-monitor/SKILL.md` teaches the object form,
`until: [{type: "match", pattern: "error:"}]`, which is the socket API
spelling, not the one `task_wait` accepts. An agent that follows that skill
through the MCP adapter would hand it a string that fails to parse.

## `StartParams.Harness` is recorded and never read

`task_start` accepts `harness` and the daemon stores it on the record. No
verb reads it back. The design's long-poll warning was meant to name the
harness in its text; until that wiring exists, the field sits in every
record with nothing consuming it.
