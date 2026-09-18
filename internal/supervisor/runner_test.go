// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"bytes"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
)

// failingWriter always fails, to simulate a sink write error such as a full
// disk or a closed pipe.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("failingWriter: write failed")
}

func TestExitZeroReportsExited(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "echo hello; exit 0"}}, &out, clock.System())
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
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("output = %q, want it to contain %q", out.String(), "hello")
	}
}

func TestNonZeroExitKeepsTheCode(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 42"}}, &bytes.Buffer{}, clock.System())
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
}

func TestSignalReportsSignaled(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "sleep 30"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := task.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	got := task.Wait()
	if got.State != StateSignaled {
		t.Fatalf("State = %q, want %q", got.State, StateSignaled)
	}
	if got.Signal != syscall.SIGTERM {
		t.Fatalf("Signal = %v, want %v", got.Signal, syscall.SIGTERM)
	}
}

func TestMissingBinaryReturnsStartError(t *testing.T) {
	_, err := Start(Spec{Command: "this-binary-does-not-exist-9f1"}, &bytes.Buffer{}, clock.System())
	if err == nil {
		t.Fatal("Start returned no error for a missing binary")
	}
}

func TestOutputCopyFailureSetsOutputErr(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "echo hello; exit 0"}}, failingWriter{}, clock.System())
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

func TestNonZeroExitLeavesOutputErrNil(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 42"}}, &bytes.Buffer{}, clock.System())
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

func TestResultRecordsATimeSpan(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 0"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.Started.IsZero() || got.Ended.IsZero() {
		t.Fatalf("Started = %v, Ended = %v, want both set", got.Started, got.Ended)
	}
	if got.Ended.Before(got.Started) {
		t.Fatalf("Ended %v is before Started %v", got.Ended, got.Started)
	}
	if got.Ended.Sub(got.Started) > time.Minute {
		t.Fatalf("span of %v is implausible", got.Ended.Sub(got.Started))
	}
}

func TestWaitIsIdempotent(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 7"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := task.Wait()
	second := task.Wait()
	if first != second {
		t.Fatalf("Wait returned %+v then %+v, want the same result", first, second)
	}
}
