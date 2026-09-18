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
