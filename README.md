# taskd

Supervised long-running tasks for coding agents. Wake on exit or silence
instead of polling with `sleep`.

Status: **daemon and MCP adapter implemented.** The daemon, its seven verbs,
the wake engine, and the MCP adapter exist, along with the `run`, `wait`, and
`mcp` CLI subcommands. The design is in
[docs/design/2026-09-17-taskd-design.md](docs/design/2026-09-17-taskd-design.md).

## The problem

Coding agents wait for long jobs by blocking a shell:

```sh
echo "Waiting 8 min"; sleep 480; kill -0 2417648 2>/dev/null && echo ALIVE || echo DEAD
```

That line costs three things:

- The agent blocks for 8 minutes. It cannot answer a question or start other
  work.
- The agent cannot compact its context while blocked, so a long watch burns
  the window.
- The liveness check reads a recycled process identifier and cannot report an
  exit code.

## The approach

A daemon owns the child process and watches its output. It wakes the agent
when something happens.

```jsonc
task_start { command: "cargo", args: ["build"], name: "87e-v2",
             patterns: [{name: "err", regex: "error:", on_match: "record"}] }
// -> { id: "8f2c-4k9z", name: "87e-v2" }

task_wait  { ids: ["8f2c-4k9z"],
             until: "exit,idle:300",
             deliver: "notify" }
```

Under Claude Code, `task_wait` returns at once with an instruction naming a
`taskd wait` command. Run that command in the background, and the agent
stays free until it exits: the harness then notifies you that the task
exited or went silent for 300 seconds. `task_status` on the same task
reports whether the `err` pattern matched. See Host support for every other
harness.

Seven tools: `task_start`, `task_wait`, `task_status`, `task_read`,
`task_search`, `task_signal`, `task_write`.

## Design points

- **The daemon is the parent.** Only the parent can read an exit code, the
  signal that ended the process, and resource usage. A watcher that polls a process
  identifier cannot.
- **Wake conditions never kill.** `elapsed` wakes the agent and leaves the task
  running. Absolute runtime caps default to off, and the output cap rotates
  the log rather than ending the job.
- **Silence is the safety net.** `idle` detects a hang. Elapsed time does not.
- **Tasks outlive sessions.** A task survives context compaction, a session
  restart, and the agent process.
- **Pseudo-terminal by default.** Measured: the Codex terminal interface
  produced 6714 bytes on a pseudo-terminal and zero bytes without one.

## Host support

| Host | Wake path | Status |
|------|-----------|--------|
| Pi | Extension pushes a turn in process | Adapter not built yet, uses long poll |
| Claude Code | Background waiter, host notifies on exit | Adapter built, skips the long poll |
| Codex | `codex queue --thread` | Adapter not built yet, uses long poll |

Long poll works everywhere and always returns a warning, because a blocked
agent is the problem this tool exists to remove.

## Skill

[`skills/task-monitor/`](skills/task-monitor/) holds the agent-facing guidance
in two forms. `SKILL.md` carries Claude frontmatter. `AGENT.md` is plain
markdown for Codex, Pi, and other hosts.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
