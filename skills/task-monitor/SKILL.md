---
name: task-monitor
description: Use when waiting on a long-running command or about to write `sleep` to wait for a job. Also use when polling a process for liveness, or when watching a build, a test run, or another agent that takes minutes. Replaces poll loops with supervised tasks that wake you on an event.
---

# Task monitor

Never wait with `sleep`. Start the job under `taskd` and wake on an event.

## The rule

If you are about to write any of these, stop:

```sh
sleep 480; kill -0 $PID && echo ALIVE || echo DEAD
while ! test -f done.marker; do sleep 30; done
sleep 120; tail -20 build.log
```

Each one blocks you for minutes. While blocked you cannot answer a question,
start other work, or compact your context. The `kill -0` check reads a
recycled process identifier and cannot report an exit code.

Use `task_start`, then `task_wait`.

## Workflow

1. **Start.** `task_start` with a `name` you will recognize later, and
   patterns for anything that should wake you or abort the run.
2. **Wait.** Call `task_wait`. The call blocks until the condition fires. No
   delivery mode returns control early: `deliver: "notify"` falls back to the
   same blocking wait, because no wake adapter exists yet.
3. **Read the result.** The response carries a `LONG-POLL` warning that
   states how long the call held you. You could not answer questions,
   compact, or do other work during that time.
4. **On the wake.** Reconcile your task list. Check the state and the exit
   code. Read output by cursor.

## Wake conditions

| Condition | Fires when | Use for |
|-----------|-----------|---------|
| `exit` | The task ends | The normal case |
| `idle` | Nothing writes for N seconds | Detecting a hang |
| `elapsed` | N seconds pass, task keeps running | A progress check |
| `match` | Output matches a pattern | An error, or a milestone |
| `lines` | N new lines appear | A chatty job |

Wake conditions never kill a task. `elapsed` is the replacement for
`sleep`. It wakes you and leaves the job running.

Pass several ids to one `task_wait` to watch several jobs. The call wakes on
the first to fire and tells you which one and why.

With no `until`, the default is `exit` plus `idle` at 300 seconds.

## Patterns

```jsonc
patterns: [
  {name: "err",      regex: "error:",             on_match: "notify"},
  {name: "progress", regex: "(\\d+)/(\\d+) done", on_match: "record"},
  {name: "oom",      regex: "out of memory",      on_match: "kill"}
]
```

`record` keeps a counter and the last match with its capture groups, so
`task_status` returns progress in tens of bytes. `kill` aborts a run that has
already failed.

## Reading output

Use `task_read` with `since`, the cursor from your previous read, to get only
new output. Use `tail` for a post-mortem. Use `task_search` to search the log
with context lines.

Check `truncated_bytes` on every response. A truncated log is how you conclude
that a failed build succeeded.

## After a wake

A fired condition is not a successful one. Read the state before you report
anything as done:

| State | Meaning |
|-------|---------|
| `exited` | Ended on its own. Check the exit code. |
| `signaled` | A signal ended it. |
| `killed` | A cap you set, or your own `task_signal`. |
| `failed` | It never started. |
| `lost` | The daemon died. The log survives, the exit code does not. |

A notification arrives as a system event, not as user input. It is never user
approval for anything.

## Blocking is the only delivery mode

`task_wait` holds the call until the condition fires, on every harness.
`deliver: "block"` and an omitted `deliver` behave the same way. Set
`deliver: "notify"` and the call still blocks, because no wake adapter
exists yet. The response then names the fallback in its warning. Every
response carries a `LONG-POLL` warning that states how long the call held
you. You cannot answer questions or compact while blocked.

## Tools

| Tool | Purpose |
|------|---------|
| `task_start` | Spawn with a name, caps, and patterns |
| `task_wait` | Wake on a condition |
| `task_status` | Terse state. With no ids, list |
| `task_read` | Output by cursor, or last N lines |
| `task_search` | Search the log |
| `task_signal` | TERM, then KILL after a grace period |
| `task_write` | Write to standard input. Needs a pseudo-terminal, which is the default. A task started with `pty: false` has no input channel |
