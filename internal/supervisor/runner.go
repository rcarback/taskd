// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/rcarback/taskd/internal/clock"
)

// Spec describes a task to run.
type Spec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
	PTY     bool
}

// Task is a running child process owned by this process.
type Task struct {
	cmd *exec.Cmd
	clk clock.Clock

	once   sync.Once
	result Result
	done   chan struct{}
}

// Start launches the task and copies its output into sink.
//
// Start returns an error only when the process fails to launch. Every other
// outcome appears in the Result from Wait.
func Start(spec Spec, sink io.Writer, clk clock.Clock) (*Task, error) {
	cmd := exec.Command(spec.Command, spec.Args...) //nolint:gosec // running the caller-chosen task command is this package's purpose
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = sink
	cmd.Stderr = sink

	t := &Task{cmd: cmd, clk: clk, done: make(chan struct{})}
	startedAt := clk.Now()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor: start %s: %w", spec.Command, err)
	}

	go t.reap(startedAt)
	return t, nil
}

// reap waits for the child and records the result exactly once.
func (t *Task) reap(startedAt time.Time) {
	_ = t.cmd.Wait()
	t.once.Do(func() {
		t.result = t.buildResult(startedAt)
		close(t.done)
	})
}

// Wait blocks until the task ends and returns the same Result on every call.
func (t *Task) Wait() Result {
	<-t.done
	return t.result
}

// Signal sends sig to the task. It is a no-op once the task has ended.
func (t *Task) Signal(sig os.Signal) error {
	if t.cmd.Process == nil {
		return fmt.Errorf("supervisor: task has no process")
	}
	if err := t.cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("supervisor: signal: %w", err)
	}
	return nil
}

// buildResult converts the exit status into a Result.
//
// A task that ended on a signal reports StateSignaled and carries no
// meaningful exit code, so callers must read State first.
func (t *Task) buildResult(startedAt time.Time) Result {
	res := Result{Started: startedAt, Ended: t.clk.Now()}

	state := t.cmd.ProcessState
	if state == nil {
		res.State = StateFailed
		return res
	}

	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		res.State = StateSignaled
		res.Signal = ws.Signal()
	} else {
		res.State = StateExited
		res.ExitCode = state.ExitCode()
	}

	if ru, ok := state.SysUsage().(*syscall.Rusage); ok {
		res.MaxRSSBytes = maxRSSBytes(ru)
	}
	return res
}
