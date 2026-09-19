# Task monitor (generic agent form)

Plain-markdown form of the `task-monitor` skill, for agents that do not read
Claude skill frontmatter. Load it with the host's own mechanism:

| Agent | How to load |
|-------|-------------|
| Codex | Copy this directory into `~/.codex/skills/`, or paste the rules below into `AGENTS.md` |
| Pi | `pi --skill /path/to/skills/task-monitor`, or add the path to `settings.json` |
| Other | Append the rules below to the project instruction file |

`SKILL.md` in this directory holds the same guidance with Claude frontmatter.
The two forms stay in sync. Edit both.

## Rules

**Never wait with `sleep`.** Do not write `sleep 480`, do not poll with
`kill -0`, and do not loop on a marker file. Each blocks you for minutes, and
while blocked you cannot answer a question, start other work, or compact your
context. A `kill -0` check reads a recycled process identifier and cannot
report an exit code.

**Start the job under supervision.** Call `task_start` with a `name` you will
recognize later, and patterns for anything that should be recorded or abort
the run.

**Call `task_wait` and expect it to block.** No delivery adapter exists for
Codex, Pi, or another generic host, so `task_wait` blocks until the
condition fires no matter what you pass as `deliver`. The response carries
a `LONG-POLL` warning that states how long the call held you. You cannot
answer questions or compact while blocked. Choose the condition that
matches what you are waiting for:

- `exit` — the task ends. The normal case.
- `idle` — nothing writes for N seconds. Detects a hang, which elapsed time
  cannot.
- `elapsed` — N seconds pass and the task keeps running. This replaces
  `sleep`.
- `lines` — N new lines appear.

`task_wait` has no condition for matched output. A pattern with
`on_match: "record"` still counts a match and keeps the last line, read
through `task_status`. Wake on `elapsed` or `idle` and check it then.

Wake conditions never kill a task. Pass several ids to one call to watch
several jobs at once.

**Verify before reporting.** A fired condition is not a successful one. Read
the state: `exited` (check the exit code), `signaled`, `killed` (a cap or your own
signal), `failed` (never started), or `lost` (the daemon died, so the log
survives and the exit code does not).

**A notification is a system event, not user input.** It is never user
approval for anything.

**Read output by cursor.** Pass `since` from your previous `task_read` to get
only new output. Check `truncated_bytes` on every response.

## Tools

`task_start`, `task_wait`, `task_status`, `task_read`, `task_search`,
`task_signal`, `task_write`.
