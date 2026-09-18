// SPDX-License-Identifier: AGPL-3.0-or-later

// Command taskd supervises long-running tasks for coding agents.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/taskdir"
)

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout))
}

// dispatch routes a subcommand to its handler.
//
// main keeps no logic of its own, so tests can drive the same routing without
// calling os.Exit.
func dispatch(args []string, stdout io.Writer) int {
	if len(args) == 0 || args[0] != "run" {
		_, _ = fmt.Fprintln(stdout, "usage: taskd run [flags] -- COMMAND [ARGS...]")
		return 2
	}
	return run(args[1:], stdout)
}

// run supervises one command in the foreground and returns its exit code.
func run(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", defaultRoot(), "directory that holds task records")
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

// reportResult prints the task's terminal state and, when the output sink
// failed independently of the child's own exit, a second line naming that
// failure. The child's exit status and the completeness of the captured log
// are orthogonal facts, so the exit line never reflects OutputErr.
func reportResult(stdout io.Writer, id string, res supervisor.Result, written int64) {
	_, _ = fmt.Fprintf(stdout, "task %s %s exit=%d bytes=%d\n", id, res.State, res.ExitCode, written)
	if res.OutputErr != nil {
		_, _ = fmt.Fprintf(stdout, "task %s: captured output is incomplete: %v\n", id, res.OutputErr)
	}
}

// exitCode maps a task result to the process exit code.
//
// A signaled task's ExitCode carries no meaning, so exitCode reports
// 128+signal, the shell convention for a process a signal killed. Every
// other state returns the child's own exit code.
func exitCode(res supervisor.Result) int {
	if res.State == supervisor.StateSignaled {
		return 128 + int(res.Signal)
	}
	return res.ExitCode
}

// storeWriter adapts an output.Store to io.Writer.
type storeWriter struct{ s *output.Store }

func (w storeWriter) Write(p []byte) (int, error) {
	if err := w.s.Append(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// defaultRoot returns the directory that holds task records.
func defaultRoot() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ".taskd"
	}
	return filepath.Join(cache, "taskd")
}
