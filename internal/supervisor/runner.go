// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"errors"
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

	// pty is set only for a task started on a pseudo-terminal. It carries
	// the copy goroutine's done channel, the master to bound a wait on a
	// lingering grandchild, and any sink write error. It is nil for a task
	// started on a plain pipe, since cmd.Wait already waits for that output
	// copy to finish and reports any of its errors directly.
	pty *ptyStream

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

	startedAt := clk.Now()

	var stream *ptyStream
	if spec.PTY {
		s, err := startPTY(cmd, sink)
		if err != nil {
			return nil, err
		}
		stream = s
	} else {
		cmd.Stdout = sink
		cmd.Stderr = sink
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("supervisor: start %s: %w", spec.Command, err)
		}
	}

	t := &Task{cmd: cmd, clk: clk, done: make(chan struct{}), pty: stream}
	go t.reap(startedAt)
	return t, nil
}

// reap waits for the child and records the result exactly once.
func (t *Task) reap(startedAt time.Time) {
	waitErr := t.cmd.Wait()
	if t.pty != nil {
		// Wait for the pty copy goroutine so the result reflects all output
		// the child produced, bounded so a grandchild that inherited the
		// pty slave and outlived the child cannot hang this forever.
		t.pty.drain(t.clk)
	}
	t.once.Do(func() {
		t.result = t.buildResult(startedAt, waitErr)
		close(t.done)
	})
}

// Wait blocks until the task ends and returns the same Result on every call.
func (t *Task) Wait() Result {
	<-t.done
	return t.result
}

// Signal sends sig to the task. Once the task has ended, the underlying
// process is already reaped and Signal returns an error reporting that the
// signal could not be delivered.
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
func (t *Task) buildResult(startedAt time.Time, waitErr error) Result {
	res := Result{Started: startedAt, Ended: t.clk.Now()}

	state := t.cmd.ProcessState
	if state == nil {
		// The task started (Start already returned successfully), so this
		// is not a launch failure: it is an exit status that became
		// unavailable, for example because something outside this package
		// reaped the child first. That is StateLost, not StateFailed;
		// StateFailed is reserved for a process that never started.
		res.State = StateLost
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

	if t.pty != nil {
		// The pty copy goroutine runs outside cmd.Wait's own bookkeeping,
		// so waitErr never carries an output-copy failure for a pty task;
		// only a sink write failure, captured on t.pty, does. A drain that
		// hit ptyDrainGrace never reaches here as a failure: the grace only
		// bounds a lingering grandchild descriptor, not the child's own
		// output, so it must never populate OutputErr.
		res.OutputErr = t.pty.writeError()
		return res
	}

	// cmd.Wait's error also reports a failure in the goroutine copying the
	// child's output into the sink. A non-zero exit surfaces here as an
	// *exec.ExitError, which is a normal outcome already captured above via
	// State and ExitCode, not an output failure. Anything else is a real
	// copy error, most commonly a sink write that failed.
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		res.OutputErr = waitErr
	}
	return res
}
