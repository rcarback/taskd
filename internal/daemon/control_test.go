// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
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

// TestSignalEscalatesToKillAfterTheGrace covers insist, the grace-then-kill
// path that is the only thing making task_signal end a task that ignores
// SIGTERM. Replacing its Signal call with a no-op left the rest of the
// package green.
//
// The daemon runs on a fake clock, so the grace is advanced rather than
// waited out: BlockUntil(1) synchronises with insist registering its timer,
// which is the only timer on this clock while the task is alive.
//
// The script execs sleep after setting the trap. An ignored disposition
// survives exec, so the task is one process that ignores SIGTERM rather than
// a shell with a sleep child: a grandchild holding the pseudo-terminal open
// after the kill would leave reap's drain waiting on a fake clock nothing
// advances again.
func TestSignalEscalatesToKillAfterTheGrace(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	d, err := New(shortRoot(t), clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Register()

	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", `trap "" TERM; exec sleep 60`},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	e, _ := d.Reg.Get(got.(StartResult).ID)

	if _, err := callVerb(t, d, "task_signal", SignalParams{ID: got.(StartResult).ID}); err != nil {
		t.Fatalf("task_signal: %v", err)
	}

	clk.BlockUntil(1)
	clk.Advance(time.Duration(defaultGraceSeconds) * time.Second)

	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: only the grace escalation can end a task that ignores SIGTERM", state)
	}
}

func TestSignalRejectsANegativeGrace(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}

	grace := -1
	_, err = callVerb(t, d, "task_signal", SignalParams{ID: got.(StartResult).ID, GraceS: &grace})
	if err == nil {
		t.Fatal("task_signal accepted grace_s = -1, which makes the kill follow the term with no grace at all")
	}
	if !strings.Contains(err.Error(), "grace_s") {
		t.Fatalf("error = %q, want it to name the field", err)
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

// TestWriteSendsInputToARunningTask asserts on output only a task that read
// its input could have produced.
//
// Asserting that the log contains the written bytes proves nothing: the
// terminal line discipline echoes every byte written to the master straight
// back to the read side, so that assertion holds even for a task that never
// reads its input at all. The shell here has to read the line and interpolate
// it, so "got=hello" can only come from the child.
func TestWriteSendsInputToARunningTask(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "read l; echo got=$l"},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID

	if _, err := callVerb(t, d, "task_write", WriteParams{ID: id, Data: "hello\n"}); err != nil {
		t.Fatalf("task_write: %v", err)
	}

	e, _ := d.Reg.Get(id)
	waitForState(t, e)

	res, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if data := res.(ReadResult).Data; !strings.Contains(data, "got=hello") {
		t.Fatalf("Data = %q, want got=hello: the task must have read the input, not just echoed it", data)
	}
}
