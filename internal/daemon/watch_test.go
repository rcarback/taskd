// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/watch"
)

func TestStartRejectsABadPattern(t *testing.T) {
	d := newDaemon(t)
	_, err := callVerb(t, d, "task_start", StartParams{
		Command:  "true",
		Patterns: []watch.Pattern{{Name: "err", Regex: "([", OnMatch: watch.ActionRecord}},
	})
	if err == nil {
		t.Fatal("task_start accepted an uncompilable pattern")
	}
	if !strings.Contains(err.Error(), "cannot compile") {
		t.Errorf("error = %q, want it to name the compile failure", err)
	}
}

func TestStatusReportsPatternCounters(t *testing.T) {
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo 1/3 done; echo 2/3 done; echo 3/3 done"},
		Patterns: []watch.Pattern{
			{Name: "progress", Regex: `(\d+)/(\d+) done`, OnMatch: watch.ActionRecord},
		},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	e, _ := d.Reg.Get(id)
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited", state)
	}

	got, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	tasks := got.(StatusResult).Tasks
	if len(tasks) != 1 {
		t.Fatalf("task_status returned %d tasks, want 1", len(tasks))
	}
	pats := tasks[0].Patterns
	if len(pats) != 1 {
		t.Fatalf("Patterns has %d entries, want 1", len(pats))
	}
	if pats[0].Count != 3 {
		t.Errorf("Count = %d, want 3", pats[0].Count)
	}
	if pats[0].LastLine != "3/3 done" {
		t.Errorf("LastLine = %q, want %q", pats[0].LastLine, "3/3 done")
	}
}

func TestPatternsFieldIsNeverNilOnTheWire(t *testing.T) {
	// A nil slice marshals to JSON null. A client iterating patterns would
	// have to test for that first.
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if got.(StatusResult).Tasks[0].Patterns == nil {
		t.Error("Patterns is nil for a task with no patterns, want an empty slice")
	}
}

func TestKillPatternEndsTheTask(t *testing.T) {
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo out of memory; sleep 60"},
		Patterns: []watch.Pattern{
			{Name: "oom", Regex: "out of memory", OnMatch: watch.ActionKill},
		},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	e, _ := d.Reg.Get(id)
	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: the oom pattern asked for it", state)
	}
}

func TestOutputStillReachesTheLogThroughTheTap(t *testing.T) {
	// The tap sits between the process and the store. A regression here
	// would silently empty every task's log.
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo hello"},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	e, _ := d.Reg.Get(id)
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited", state)
	}

	one := 1
	got, err := callVerb(t, d, "task_read", ReadParams{ID: id, Tail: &one})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if !strings.Contains(got.(ReadResult).Data, "hello") {
		t.Errorf("Data = %q, want it to contain %q", got.(ReadResult).Data, "hello")
	}
}
