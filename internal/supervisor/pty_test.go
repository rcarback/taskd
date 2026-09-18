// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
)

// tty exits 0 when it runs on a terminal, and 1 otherwise.
const ttyProbe = "if [ -t 1 ]; then echo TTY; else echo PIPE; fi"

func TestPTYGivesTheChildATerminal(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", ttyProbe}, PTY: true}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := task.Wait(); got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if !strings.Contains(out.String(), "TTY") {
		t.Fatalf("output = %q, want it to contain TTY", out.String())
	}
}

func TestWithoutPTYTheChildSeesAPipe(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", ttyProbe}, PTY: false}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := task.Wait(); got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if !strings.Contains(out.String(), "PIPE") {
		t.Fatalf("output = %q, want it to contain PIPE", out.String())
	}
}

func TestPTYStillReportsTheExitCode(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 9"}, PTY: true}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := task.Wait()
	if got.State != StateExited || got.ExitCode != 9 {
		t.Fatalf("Result = %+v, want exited with code 9", got)
	}
}

func TestPTYSinkWriteFailureSetsOutputErr(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "echo hello; exit 0"}, PTY: true}, failingWriter{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if got.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", got.ExitCode)
	}
	if got.OutputErr == nil {
		t.Fatal("OutputErr = nil, want a non-nil error when the sink write fails")
	}
}

func TestPTYNonZeroExitLeavesOutputErrNil(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 42"}, PTY: true}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if got.ExitCode != 42 {
		t.Fatalf("ExitCode = %d, want 42", got.ExitCode)
	}
	if got.OutputErr != nil {
		t.Fatalf("OutputErr = %v, want nil for a normal non-zero exit with a working sink", got.OutputErr)
	}
}

// TestPTYDrainReturnsAfterGraceExpires is a unit test of the bounded-wait
// helper alone, with a done channel that never closes on its own. A real
// grandchild holding the pty slave open does not hang on Darwin, so an
// end-to-end reproduction would pass vacuously here and only fail in CI on
// Linux; this test exercises the timeout path directly and deterministically
// instead, via the fake clock.
func TestPTYDrainReturnsAfterGraceExpires(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	done := make(chan struct{}) // deliberately never closed on its own
	returned := make(chan struct{})
	var graceExpired bool

	go func() {
		drainWithGrace(clk, done, func() { graceExpired = true })
		close(returned)
	}()

	clk.BlockUntil(1)
	clk.Advance(ptyDrainGrace)

	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("drainWithGrace did not return after the grace period elapsed")
	}
	if !graceExpired {
		t.Fatal("drainWithGrace did not call onGraceExpired when the grace period elapsed")
	}
}
