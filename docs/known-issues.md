# Known issues

Open items carried out of the daemon and protocol work. Each one is recorded
here because a review found it, decided not to fix it on that branch, and gave
a reason.

## A test deadlocks at about 0.5 percent under parallel load

`TestReadReachesALiveTaskThroughTheOpenStore` in
`internal/daemon/read_test.go` hangs. It does not fail slowly. Raising the
10 second bound does not help: an instrumented run waited a further 100
seconds and never completed.

The test starts a task as `cat <fifo>`, then opens the write end of the FIFO
and closes it to hand `cat` an end of input. At the hang, `cat` is still
alive and blocked in `read`, which means a write end of that FIFO is still
open somewhere. The daemon side is correct. `supervisor.(*Task).reap` waits
in `Wait4` for a child that has not exited, and the pseudo-terminal
copy goroutine is in a normal read.

This is a defect in the test, not in the product. No production task reads a
FIFO that the daemon arranges, and nothing in `internal/daemon` or
`internal/supervisor` is stuck.

Measured at roughly 5 failures in 1000 runs under 8 to 24 concurrent test
binaries. It reproduces on the commit before the fix round as well, so it
predates that work.

The holder of the write end is not identified. `lsof` reported nothing for
the FIFO path on the machine used, including for `cat`, which held the read end.

One lead, untested. A process forked by another test in the same binary may
inherit the write end before anything closes it. The failure appears only
under parallel load, and load raises the number of concurrent forks. Go sets
close-on-exec on the files it opens, so this needs proof before anyone acts
on it.

Expect this to fail in continuous integration.

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
