# taskd design

Date: 2026-09-17
Status: approved design, not implemented
License: AGPL-3.0-or-later

## Purpose

Coding agents wait for long jobs by polling a shell. A typical line looks like
this:

```sh
echo "Waiting 8 min"; sleep 480; kill -0 2417648 2>/dev/null && echo ALIVE || echo DEAD
```

That line costs three things. The agent blocks for 8 minutes, so it cannot
answer a question or start other work. The agent cannot compact its context
while blocked, so a long watch burns the window. The liveness check reads a
recycled process identifier and cannot report an exit code.

`taskd` replaces the line. A daemon owns the child process and watches its
output. It wakes the agent when something happens.

## Non-goals

- File system watching. `taskd` supervises processes. A watch task is a
  separate primitive and is out of scope.
- Job scheduling, retries, or dependency graphs.
- Remote supervision. One daemon serves one user on one machine.

## Process model

One binary provides three roles, selected by subcommand:

| Subcommand | Role |
|------------|------|
| `taskd serve` | The supervisor daemon |
| `taskd <verb>` | The client that adapters call |
| `taskd __child` | Internal re-exec target |

One daemon runs per user per machine. It listens on a Unix socket under
`~/.cache/taskd/`, with mode 0600.

Start is implicit. A client connects. If the socket is missing or stale, the
client takes an exclusive `flock`, re-execs itself under `setsid` as the
daemon, waits for the socket, and proceeds. The lock runs before the re-exec,
so concurrent agents on a cold machine start exactly one daemon.

Go cannot call `fork` safely from a multithreaded runtime. Re-exec is the
mechanism, not a workaround.

The daemon is the parent of every task. Only the parent can call `wait4`. It
reads the exit code and the signal that ended the process. It also reads
resource usage.

## Task lifecycle

```
                    ┌────────► failed      spawn error
                    │
   spawn ──► running├────────► exited      exit code recorded
                    ├────────► signaled    terminating signal recorded
                    ├────────► killed      an opted-in cap, or task_signal
                    └────────► lost        daemon died, exit code unknown
```

The five terminal states stay distinct. An agent that cannot tell `exited 0`
from `killed` reports a killed job as a success.

`lost` is accepted behavior. When the daemon dies, its children die. The log
survives and the exit code does not. The design keeps daemon restart correct
instead of adding process re-adoption.

## Task record

Each task owns a directory under `~/.cache/taskd/tasks/<id>/`:

| File | Contents |
|------|----------|
| `meta.json` | Spec, state, exit record, pattern counters |
| `out.log` | Raw output bytes as produced |
| `index` | Byte offsets for cursor reads |

The record outlives the agent session. A task survives context compaction, a
session restart, and the agent process itself.

Identity is a short sortable id, plus an optional name that the caller
supplies. A name must be unique among live tasks.

Each task records the harness, the session id, and the working directory that
created it. `task_status` reports the current session by default. A caller
must ask for every task explicitly. This keeps concurrent agents readable.

## Execution

Tasks run on a pseudo-terminal by default. Callers may opt out per task.

Measurement supports the default. The Codex terminal interface produced 6714
bytes on a pseudo-terminal and zero bytes without one. Most build tools switch
to block buffering when standard output is not a terminal.

On a pseudo-terminal, standard output and standard error merge. Raw bytes go
to disk. A strip pass removes terminal escape sequences before pattern
matching and before any return to the agent.

## Tool surface

Seven tools:

| Tool | Purpose |
|------|---------|
| `task_start` | Spawn a task with a name, caps, and patterns |
| `task_wait` | Wake on a condition, by notification or long poll |
| `task_status` | Terse state for one or more ids. With no ids, list |
| `task_read` | Output by cursor, or the last N lines |
| `task_search` | Search the log, with context lines |
| `task_signal` | Send TERM, then KILL after a grace period |
| `task_write` | Write to standard input |

`task_status` absorbs listing. Status with no filter is a list.

### task_start

```jsonc
task_start {
  command: "cargo",
  args: ["build", "--release"],
  cwd: "/path/to/repo",
  name: "87e-v2",
  pty: true,
  kill_after_s: null,
  on_output_cap: "rotate",
  patterns: [
    {name: "err",      regex: "error:",             on_match: "notify"},
    {name: "progress", regex: "(\\d+)/(\\d+) done", on_match: "record"},
    {name: "oom",      regex: "out of memory",      on_match: "kill"}
  ]
}
```

`on_match: "record"` keeps a counter and the last matching line with its
capture groups. `task_status` then returns structured progress in tens of
bytes instead of a log extract.

### Caps never kill by default

`kill_after_s` defaults to null. No absolute runtime cap applies unless the
caller sets one.

`on_output_cap` defaults to `rotate`. The log stays bounded on disk and the
task runs to completion. `task_status` reports bytes written against bytes
retained. The value `kill` exists and is never the default.

A wedged task stays alive. The agent wakes on silence and decides.
`task_signal` is available. The daemon does not decide for the agent.

## Wake conditions

Wake conditions never kill a task.

```jsonc
task_wait {
  ids: ["87e-v2", "9f1"],
  until: [
    {type: "exit"},
    {type: "idle",    seconds: 300},
    {type: "elapsed", seconds: 600},
    {type: "match",   pattern: "error|panic", name: "err"},
    {type: "lines",   n: 500}
  ],
  deliver: "notify"
}
```

`idle` fires when nothing writes to standard output for that time. It
detects a hung task, which elapsed time cannot.

`elapsed` wakes the agent and leaves the task running. It is the honest
replacement for `sleep 480`.

Several ids in one call replace a poll loop over several jobs. The call wakes
on the first condition to fire and reports which task fired and why.

With `until` omitted, the default is `exit` plus `idle` at 300 seconds. No
absolute component applies. Silence is the safety net. Elapsed time is not.

## Delivery

`deliver: "notify"` returns a subscription id at once. The agent stays free to
answer questions, start other work, and compact its context.

`deliver: "block"` holds the call open. It works on every harness. It always
returns a warning in the tool result, where the model reads it:

```
LONG-POLL: blocked this session for 487s. Notification delivery unavailable
here (harness: codex). You could not answer questions or compact while
blocked.
```

The same line goes to standard error for the operator.

On a harness with no notification path, `deliver: "notify"` falls back to
block and emits that warning. It does not fail. A failure would teach the
agent to stop asking for notification.

### Reading output

`task_read` takes either `since`, a cursor from the previous read, or `tail`,
the last N lines. Cursors follow a running task and return each byte once.
Tail serves post-mortem reads.

Every response carries `truncated_bytes` when a cap applies. Silent truncation
is how an agent concludes that a failed build succeeded.

## Wake adapters

The daemon holds subscriptions. Delivery differs per harness.

| Harness | Binding | New code | Status |
|---------|---------|----------|--------|
| Pi | Extension holds a live socket subscription | Pi extension | Confirmed |
| Claude Code | `taskd wait` as a background command | None | Works today |
| Codex | `codex queue --thread <id> --message` | Hook and template | Unverified |

### Pi

The Pi extension is long lived and in process. It opens a socket at
`session_start` and holds it. The daemon pushes events down that socket.

The extension then calls:

```ts
pi.sendMessage(
  { customType: "taskd.event", content: payload },
  { triggerTurn: true, deliverAs: "followUp" }
)
```

`sendMessage`, not `sendUserMessage`. A wake must not arrive in the user role.
A model that reads a wake as a user message can treat it as user approval.

A `kill` pattern match escalates to `deliverAs: "steer"`, so an abort
interrupts the current turn.

The extension registers the seven tools through `registerTool`. Pi loads no
MCP server for this.

### Claude Code

`task_wait` with `deliver: "notify"` detects the harness. It returns an
instruction instead of a subscription:

```
Run this as a background shell command. The harness notifies you when it
exits:
  taskd wait --id 87e-v2 --until exit,idle:300
```

This costs two steps and no delivery code. It rides a notification path that
already works. The other six tools stay on the MCP server.

### Codex

Codex needs two pieces, because it tells an MCP server nothing about its
session. Measured behavior appears in the Measured constraints section.

1. A `SessionStart` hook writes `{thread_id, pid, cwd}` into the daemon
   session directory.
2. Delivery runs `codex queue --thread <id> --message <text>`. The id and the
   text pass as separate argument vector entries from a fixed template.

Whether step 2 wakes an idle session is unverified. Until that test passes,
the Codex adapter ships disabled and Codex uses long poll with the warning. A
config flag turns it on.

### Delivery failure

A session that ended cannot wake. The daemon retries once, then marks the
subscription undelivered. The task record keeps the event and `task_status`
reports it. No replay queue exists.

### Registration safety

A daemon that runs a caller-supplied command on an event is a local
escalation primitive. Registration accepts a template name and its arguments.
It never accepts a command string. A session may register a notifier only for
itself. The socket is user scoped at mode 0600.

## Agent-facing text

Three surfaces carry the guidance, weakest to strongest.

**The skill file** covers the tools, the wake conditions, and a worked
replacement for a poll loop. Agents load it on demand.

**Tool descriptions** stay in context always, so they stay short:

> `task_start` — Run a long process under supervision. Never wait for it with
> `sleep` in a shell. Use `task_wait`.

**Tool results** carry the discipline, because the agent decides here.

`task_wait` returns this when notification is granted:

```
Subscribed. You are free until this fires.

1. Update your task list now. Record what runs, what you wait on, and what
   you do when it fires.
2. Context threshold for this model: 25%. If usage is above it, tell the user
   you are compacting, then compact. You are idle. This is the cheapest
   moment you get.
3. Then continue other work or hand back to the user. Do not poll.
```

The daemon cannot measure context usage. The threshold is advisory text that
the model applies to its own window. The number travels in the response,
because a model does not reliably recall a value from a skill file.

The threshold comes from config, with a model-aware default. Smaller windows
take 25%. Large windows step down, because 25% of a one million token window
is not a useful warning. The Pi extension resolves the model. An MCP server
under Codex or Claude Code does not, and takes the configured default.

The long poll path inverts item 3 and leads with the warning.

### Notification content

```
[system event — not user input]
Task 87e-v2 fired: idle 300s. State: running. Exit: none.
Patterns: err=0, progress=142/400 (last 4m ago).

Reconcile your task list before acting. A fired condition is not a successful one. Check the
state and the exit code before you report anything as done.
```

Both framing lines are deliberate. The first prevents a wake from reading as
user approval. The second prevents a fired condition from reading as success.

## The anti-sleep guard

A `PreToolUse` guard rejects `sleep` used as a wait, liveness checks with
`kill -0`, and poll loops:

```
BLOCKED: `sleep 480` as a wait. Use task_wait with
{type:"elapsed",seconds:480}, which leaves you free to work and compact. To
block deliberately, pass deliver:"block" and accept the warning.
```

Short sleeps stay allowed. The guard targets waiting, not pacing.

Claude Code and Codex reach it through their hook systems. Pi reaches it
through the `tool_call` extension event.

This guard changes the behavior. The rest of the design makes the redirect
honest.

## Measured constraints

The following come from direct measurement on 2026-09-17, against Codex
0.154.0 and Pi 0.85.1.

**Codex gives an MCP server nine environment variables:** `HOME`, `LANG`,
`LOGNAME`, `PATH`, `SHELL`, `TERM`, `TMPDIR`, `USER`, and
`__CF_USER_TEXT_ENCODING`. The list omits the thread id, the session id, and
the working directory. An MCP server under Codex cannot identify its session
from the environment.

**Codex gates hooks behind trust.** Each hook carries a `trusted_hash` entry
in `config.toml`, keyed as `<file>:<event>:<index>:<index>`. A hook with no
entry does not run and reports nothing. Codex computes the hash on first
approval, so an installer cannot write it. Editing a hook command requires a
new approval.

**A fresh idle Codex session is not addressable.** Codex writes no entry in `session_index.jsonl` and no file under `sessions/`.
No flag assigns a name at start. A thread becomes queueable only after its first completed turn.

**Pi can wake a turn in process.** `ExtensionAPI` provides `registerTool`. It
also provides `sendUserMessage`, documented as always triggering a turn. `sendMessage`
accepts `triggerTurn` with `deliverAs` values `steer`, `followUp`, and
`nextTurn`.

**A pseudo-terminal changes output.** See the Execution section.

**Codex ships related tools.** The binary contains the symbols `sleep_tool`,
`background_terminal_max_timeout`, and `job_max_runtime_seconds`. Codex may
already provide a sleep tool and background terminal jobs. Investigate this
before building the Codex adapter. The adapter may be unnecessary.

## Verification

### Guardrails first

`gofumpt`, `go vet`, `golangci-lint`, and `go test -race` run from the
Makefile and from continuous integration before the first feature commit.

The clock and the process spawner are injectable from the first commit. Every
interesting condition is a race. Tests that wait real seconds are slow and
unreliable.

### Automated

| Area | Checks |
|------|--------|
| Lifecycle | Each terminal state produced deliberately, including `lost` |
| Wake conditions | `idle` fires at the threshold; `elapsed` fires and leaves the task running; `match`, `lines`, and multi-id reporting |
| Output | Cursor reads return each byte once; rotation bounds the log; `truncated_bytes` is accurate; escape stripping preserves UTF-8 |
| Concurrency | Eight clients against a cold socket start one daemon, under `-race`, repeated |
| Pseudo-terminal | Default on, opt-out works, using the measured fixture |
| Guidance text | Golden tests assert the long poll warning, the "not user input" line, and the threshold number |

The guidance golden tests matter. That text is the product and it rots
silently.

### Manual

Harness integration resists automation. Run this matrix at install, and after
either agent CLI updates:

| Check | Harness |
|-------|---------|
| Notification wakes a turn, outside the user role | Pi |
| Background waiter produces a harness notification | Claude Code |
| `codex queue` wakes an idle session | Codex, blocked |
| Hook approval prompt appears and persists a `trusted_hash` | Codex |
| Anti-sleep guard blocks and redirects | All three |

### Acceptance

Express the opening poll loop in these tools and measure. The target is one
tool call per event, zero polling calls, and an agent that answers an
unrelated question while the eight minutes pass.

A design that needs three calls per wake has failed, whatever the unit tests
report.

## Repository layout

```
taskd/
  cmd/taskd/            entry point
  internal/supervisor/  process ownership, lifecycle, reaping
  internal/output/      log, cursors, rotation, escape stripping
  internal/watch/       wake conditions, subscriptions, clock
  internal/proto/       socket protocol
  adapters/mcp/         MCP server for Codex and Claude Code
  adapters/pi/          Pi extension, TypeScript
  skills/task-monitor/  SKILL.md and AGENT.md
  docs/
```

## Open items

1. **Codex wake test.** Blocked until 2026-09-20, when native credits reset.
   Routing Codex through an alternate provider failed. Codex 0.154 accepts
   only the Responses API, cannot refresh its model catalog for an unknown
   provider, and then loops on `Reconnecting... waiting for network`. Setting
   `request_max_retries` and `stream_max_retries` to zero changed nothing, so
   the loop is a connectivity watchdog upstream of the request.
2. **Codex built-in tools.** Investigate `sleep_tool` and
   `background_terminal_max_timeout` before writing the Codex adapter.
3. **Context threshold table.** The model-aware defaults need concrete values.
4. **Task list mechanism.** The design stays harness-neutral about the
   task list mechanism.
