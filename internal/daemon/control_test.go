// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/supervisor"
)

func TestSignalEndsATaskAndReportsKilled(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID

	if _, err := callVerb(t, d, "task_signal", SignalParams{ID: id}); err != nil {
		t.Fatalf("task_signal: %v", err)
	}

	e, _ := d.Reg.Get(id)
	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: a client asked for this", state)
	}
	if e.Record().Exit != nil {
		t.Fatal("Exit is set for a killed task, want nil")
	}
}

func TestSignalOnAFinishedTaskFails(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "exit 0")

	if _, err := callVerb(t, d, "task_signal", SignalParams{ID: id}); err == nil {
		t.Fatal("task_signal succeeded on a task that already ended")
	}
}

// TestANonPTYTaskReadingStdinReachesEOF pins the one thing a non-PTY task's
// input arrangement has to guarantee: a task that reads standard input and
// that nobody ever writes to must still end. cat with a stdin pipe no verb
// can close blocks forever and holds its name against reuse until somebody
// signals it; cat with os/exec's default /dev/null sees end of input at once
// and exits.
func TestANonPTYTaskReadingStdinReachesEOF(t *testing.T) {
	d := newDaemon(t)
	noPTY := false
	got, err := callVerb(t, d, "task_start", StartParams{Command: "cat", PTY: &noPTY})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}

	e, _ := d.Reg.Get(got.(StartResult).ID)
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited: a non-PTY task reading stdin must see EOF, not block forever", state)
	}
}

// TestWriteToANonPTYTaskFails records the cost of the arrangement above: a
// non-PTY task has no input channel, and task_write says so rather than
// reporting a write nothing can receive.
func TestWriteToANonPTYTaskFails(t *testing.T) {
	d := newDaemon(t)
	noPTY := false
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "sleep 60"}, PTY: &noPTY,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}

	_, err = callVerb(t, d, "task_write", WriteParams{ID: got.(StartResult).ID, Data: "hello\n"})
	if err == nil {
		t.Fatal("task_write succeeded for a non-PTY task, which has no input channel")
	}
	if !strings.Contains(err.Error(), "input channel") {
		t.Fatalf("error = %q, want it to name the missing input channel", err)
	}
}

func TestWriteSendsInputToARunningTask(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{Command: "cat"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID

	if _, err := callVerb(t, d, "task_write", WriteParams{ID: id, Data: "hello\n"}); err != nil {
		t.Fatalf("task_write: %v", err)
	}
	if _, err := callVerb(t, d, "task_write", WriteParams{ID: id, Data: "\x04"}); err != nil {
		t.Fatalf("task_write EOT: %v", err)
	}

	e, _ := d.Reg.Get(id)
	waitForState(t, e)

	res, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if !strings.Contains(res.(ReadResult).Data, "hello") {
		t.Fatalf("Data = %q, want the input echoed back", res.(ReadResult).Data)
	}
}
