// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"bytes"
	"errors"
	"io"
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

func TestNonZeroExitWithFailingSinkStillSetsOutputErr(t *testing.T) {
	// os/exec's cmd.Wait sets its returned error to *exec.ExitError as soon
	// as the child exits non-zero, before it ever looks at a copy
	// goroutine's error, so a naive OutputErr derived from cmd.Wait's error
	// alone would silently drop a real sink write failure whenever the
	// child also happened to exit non-zero.
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "echo hello; exit 3"}}, failingWriter{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if got.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", got.ExitCode)
	}
	if got.OutputErr == nil {
		t.Fatal("OutputErr = nil, want a non-nil error when the sink write fails, even though the child also exited non-zero")
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

func TestResultReportsNonZeroMaxRSS(t *testing.T) {
	// maxRSSBytes is platform-specific arithmetic (rusage_darwin.go,
	// rusage_linux.go) that nothing else in this suite exercises against a
	// real process; this guards against MaxRSSBytes silently staying 0.
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 0"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.MaxRSSBytes <= 0 {
		t.Fatalf("MaxRSSBytes = %d, want a positive value for a real process", got.MaxRSSBytes)
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

func TestWriteSendsInputToAPipeTask(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "cat", Stdin: true}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := task.Write([]byte("ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := task.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}

	if got := task.Wait(); got.State != StateExited {
		t.Fatalf("State = %q, want exited", got.State)
	}
	if !strings.Contains(out.String(), "ping") {
		t.Fatalf("output = %q, want the input echoed back", out.String())
	}
}

func TestWriteWithoutAnInputChannelFails(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 0"}}, io.Discard, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	task.Wait()

	if _, err := task.Write([]byte("x")); err == nil {
		t.Fatal("Write succeeded on a task with no input channel")
	}
}

func TestWriteSendsInputToAPTYTask(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "cat", PTY: true}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := task.Write([]byte("ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// A terminal ends input on EOT rather than on a closed descriptor.
	if _, err := task.Write([]byte{4}); err != nil {
		t.Fatalf("Write EOT: %v", err)
	}
	task.Wait()
	if !strings.Contains(out.String(), "ping") {
		t.Fatalf("output = %q, want the input echoed back", out.String())
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
