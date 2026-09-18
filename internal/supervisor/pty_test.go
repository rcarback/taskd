// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"bytes"
	"strings"
	"testing"

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
