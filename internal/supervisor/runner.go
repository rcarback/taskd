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

// errRecordingWriter wraps sink and records the first error sink.Write
// returns, so buildResult can surface a failed output write as
// Result.OutputErr on either path Start can take.
//
// A non-zero exit makes cmd.Wait return an *exec.ExitError before it ever
// looks at a copy goroutine's own error (see os/exec's awaitGoroutines), so
// deriving OutputErr from cmd.Wait's error would silently drop a real sink
// write failure whenever the child also happened to exit non-zero. This
// type observes the write directly instead, independent of cmd.Wait.
//
// os/exec also collapses cmd.Stdout and cmd.Stderr onto a single copy
// goroutine only when they are the identical interface value. Start
// constructs exactly one errRecordingWriter per task and uses that same
// pointer everywhere a sink is needed, so Write is never called by two
// goroutines at once; the mutex here only guards a Write against a
// concurrent Err() call.
type errRecordingWriter struct {
	sink io.Writer

	mu  sync.Mutex
	err error
}

func (w *errRecordingWriter) Write(p []byte) (int, error) {
	n, err := w.sink.Write(p)
	if err != nil {
		w.mu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.mu.Unlock()
	}
	return n, err
}

// Err reports the first error a write into the wrapped sink returned, or
// nil.
func (w *errRecordingWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Task is a running child process owned by this process.
type Task struct {
	cmd *exec.Cmd
	clk clock.Clock

	// pty is set only for a task started on a pseudo-terminal, so reap can
	// bound how long it waits for its copy goroutine. It is nil for a task
	// started on a plain pipe, since cmd.Wait already waits for that copy to
	// finish on its own.
	pty *ptyStream
	// outputRec records the first error writing the task's output into
	// sink, on either path.
	outputRec *errRecordingWriter

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
	rec := &errRecordingWriter{sink: sink}

	var stream *ptyStream
	if spec.PTY {
		s, err := startPTY(cmd, rec)
		if err != nil {
			return nil, err
		}
		stream = s
	} else {
		cmd.Stdout = rec
		cmd.Stderr = rec
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("supervisor: start %s: %w", spec.Command, err)
		}
	}

	t := &Task{cmd: cmd, clk: clk, done: make(chan struct{}), pty: stream, outputRec: rec}
	go t.reap(startedAt)
	return t, nil
}

// reap waits for the child and records the result exactly once.
func (t *Task) reap(startedAt time.Time) {
	_ = t.cmd.Wait()
	if t.pty != nil {
		// Wait for the pty copy goroutine so the result reflects all output
		// the child produced, bounded so a grandchild that inherited the
		// pty slave and outlived the child cannot hang this forever.
		t.pty.drain(t.clk)
	}
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
func (t *Task) buildResult(startedAt time.Time) Result {
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

	// t.outputRec, not cmd.Wait's own error, is the single source of
	// OutputErr on both the pipe and pty paths — see the type's doc comment
	// for why cmd.Wait's error cannot be trusted for this. A drain that hit
	// ptyDrainGrace on a pty task never affects this: the grace only bounds
	// a lingering grandchild descriptor, not the child's own output.
	res.OutputErr = t.outputRec.Err()
	return res
}
