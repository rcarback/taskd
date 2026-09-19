// SPDX-License-Identifier: AGPL-3.0-or-later

// Command taskd supervises long-running tasks for coding agents.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	mcpadapter "github.com/rcarback/taskd/adapters/mcp"
	"github.com/rcarback/taskd/internal/client"
	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/taskdir"
	"github.com/rcarback/taskd/internal/watch"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout))
}

// dispatch routes a subcommand to its handler.
//
// main keeps no logic of its own, so tests can drive the same routing without
// calling os.Exit.
func dispatch(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stdout, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return run(args[1:], stdout)
	case "wait":
		return wait(args[1:], stdout)
	case "mcp":
		return runMCP(args[1:], stdout)
	case "serve":
		return serve(args[1:], stdout)
	default:
		_, _ = fmt.Fprintln(stdout, usage)
		return 2
	}
}

const usage = "usage: taskd run [flags] -- COMMAND [ARGS...]\n" +
	"       taskd wait --id ID [--id ID...] [--until exit,idle:300]\n" +
	"       taskd mcp [--root DIR] [--harness NAME]\n" +
	"       taskd serve [--root DIR]"

// serve runs the daemon in the foreground until a signal ends it.
func serve(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", paths.Root(), "directory that holds task records")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	d, err := daemon.New(*root, clock.System())
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	d.Register()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := d.Serve(ctx); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	return 0
}

// runMCP serves the tools over standard input and output.
//
// Nothing else may write to stdout while this runs: the transport is
// newline-delimited JSON on that stream, and a stray line corrupts it. Errors
// go to the caller's stdout only after Run returns.
func runMCP(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", paths.Root(), "directory that holds task records")
	harness := fs.String("harness", string(mcpadapter.HarnessGeneric),
		"agent runtime: generic, claude-code, or codex")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	h, err := mcpadapter.ParseHarness(*harness)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := mcpadapter.Run(ctx, *root, h); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	return 0
}

// run supervises one command in the foreground and returns its exit code.
func run(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", paths.Root(), "directory that holds task records")
	noPTY := fs.Bool("no-pty", false, "run without a pseudo-terminal")
	maxOutput := fs.Int64("max-output", 8<<20, "bytes of output to retain")

	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}
	command := fs.Args()
	if len(command) == 0 {
		_, _ = fmt.Fprintln(stdout, "taskd: no command given")
		return 2
	}

	dir, id, err := taskdir.New(*root)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	store, err := output.Open(filepath.Join(dir, "out.log"), *maxOutput)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	spec := supervisor.Spec{
		Command: command[0],
		Args:    command[1:],
		Env:     os.Environ(),
		PTY:     !*noPTY,
	}

	// Pass storeWriter as the single interface value used for both stdout
	// and stderr: supervisor.Start collapses the two onto one copy goroutine
	// only when it sees the identical value, and output.Store's own mutex
	// already makes that value safe to write from that goroutine after
	// Wait returns.
	task, err := supervisor.Start(spec, storeWriter{store}, clock.System())
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	res := task.Wait()
	reportResult(stdout, id, res, store.Written())
	return exitCode(res)
}

// idList collects a repeated --id flag.
type idList []string

func (l *idList) String() string { return strings.Join(*l, ",") }

func (l *idList) Set(v string) error {
	if v == "" {
		return errors.New("an id cannot be empty")
	}
	*l = append(*l, v)
	return nil
}

// wait blocks until a condition fires and prints the result as JSON.
//
// This is the Claude Code wake path. The agent runs it as a background
// command and the harness notifies the session when it exits, so taskd
// needs no delivery code of its own for that harness.
func wait(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ids idList
	fs.Var(&ids, "id", "task id to wait on; repeat for several")
	until := fs.String("until", "", "conditions, for example exit,idle:300 (default exit,idle:300)")
	root := fs.String("root", paths.Root(), "directory that holds task records")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n%s\n", err, usage)
		return 2
	}
	if len(ids) == 0 {
		_, _ = fmt.Fprintf(stdout, "taskd: wait needs at least one --id\n%s\n", usage)
		return 2
	}

	conds, err := watch.ParseUntil(*until)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	params, err := json.Marshal(daemon.WaitParams{IDs: ids, Until: conds, Deliver: "block"})
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	resp, err := client.Call(*root, proto.Request{Verb: proto.VerbWait, Params: params})
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	if !resp.OK {
		_, _ = fmt.Fprintf(stdout, "taskd: %s\n", resp.Error)
		return 1
	}

	// The raw result goes to stdout so the agent reads the same fields the
	// socket carried, including the long poll warning.
	_, _ = fmt.Fprintf(stdout, "%s\n", resp.Result)
	return 0
}

// reportResult prints the task's terminal state and, when the output sink
// failed independently of the child's own exit, a second line naming that
// failure. The child's exit status and the completeness of the captured log
// are orthogonal facts, so the exit line never reflects OutputErr.
func reportResult(stdout io.Writer, id string, res supervisor.Result, written int64) {
	// exitCode(res), not the raw res.ExitCode, because ExitCode carries no
	// meaning for a signaled (or lost, failed, killed) task, and printing
	// it as-is would report exit=0 for a task a signal killed.
	_, _ = fmt.Fprintf(stdout, "task %s %s exit=%d bytes=%d\n", id, res.State, exitCode(res), written)
	if res.OutputErr != nil {
		_, _ = fmt.Fprintf(stdout, "task %s: captured output is incomplete: %v\n", id, res.OutputErr)
	}
}

// exitCode maps a task result to the process exit code.
//
// Only StateExited carries a meaningful ExitCode. StateSignaled reports
// 128+signal, the shell convention for a process a signal killed.
// StateLost, StateFailed, and StateKilled report plain 1: their ExitCode is
// the unset zero value, and returning it as-is would report success for an
// outcome that is unknown, never started, or terminated by us. StateLost is
// unreachable from this command today but becomes live once Plan 2 adds a
// daemon that can lose track of a task across a restart; StateKilled's
// eventual convention belongs to Plan 2's kill caps. Both are covered here
// so exitCode has no wrong answer to fall into later.
func exitCode(res supervisor.Result) int {
	switch res.State {
	case supervisor.StateExited:
		return res.ExitCode
	case supervisor.StateSignaled:
		return 128 + int(res.Signal)
	case supervisor.StateLost, supervisor.StateFailed, supervisor.StateKilled:
		return 1
	default:
		// StateRunning, or any future state Wait never actually returns:
		// there is still no meaningful exit code to report.
		return 1
	}
}

// storeWriter adapts an output.Store to io.Writer.
type storeWriter struct{ s *output.Store }

func (w storeWriter) Write(p []byte) (int, error) {
	if err := w.s.Append(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
