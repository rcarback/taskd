// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/supervisor"
)

// newDaemon returns a registered daemon on a temporary root, without a
// listener the test drives itself, so a verb test calls handlers directly.
//
// shortRoot, not t.TempDir(), because New binds the taskd socket as part of
// preparing the root, and several of this file's test names are long enough
// that t.TempDir()'s embedded test name would push the socket path over
// macOS's 104-byte sockaddr_un.sun_path limit — see shortRoot's doc comment
// in daemon_test.go.
func newDaemon(t *testing.T) *Daemon {
	t.Helper()
	d, err := New(shortRoot(t), clock.System())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Register()
	return d
}

// callVerb runs one handler with params encoded from v.
func callVerb(t *testing.T, d *Daemon, verb string, v any) (any, error) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	d.mu.RLock()
	h := d.handlers[proto.Verb(verb)]
	d.mu.RUnlock()
	if h == nil {
		t.Fatalf("no handler for %s", verb)
	}
	return h(b)
}

// waitForState blocks until the entry reaches a terminal state.
func waitForState(t *testing.T, e *Entry) supervisor.State {
	t.Helper()
	select {
	case <-e.Done():
		return e.Record().State
	case <-time.After(10 * time.Second):
		t.Fatal("task did not reach a terminal state")
		return ""
	}
}

func TestStartRunsATaskAndRecordsItsExit(t *testing.T) {
	d := newDaemon(t)

	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo hi; exit 7"}, Name: "job",
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	res, ok := got.(StartResult)
	if !ok {
		t.Fatalf("result type %T, want StartResult", got)
	}

	e, ok := d.Reg.Get(res.ID)
	if !ok {
		t.Fatal("the started task is not in the registry")
	}
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited", state)
	}
	rec := e.Record()
	if rec.Exit == nil || *rec.Exit != 7 {
		t.Fatalf("Exit = %v, want 7", rec.Exit)
	}
	if rec.Written == 0 {
		t.Fatal("Written = 0, want the bytes the task produced")
	}
}

func TestStartRejectsADuplicateLiveName(t *testing.T) {
	d := newDaemon(t)
	p := StartParams{Command: "sh", Args: []string{"-c", "sleep 30"}, Name: "job"}

	if _, err := callVerb(t, d, "task_start", p); err != nil {
		t.Fatalf("first task_start: %v", err)
	}
	if _, err := callVerb(t, d, "task_start", p); err == nil {
		t.Fatal("task_start accepted a duplicate name while the first task runs")
	}
}

func TestStartRejectsAnEmptyCommand(t *testing.T) {
	d := newDaemon(t)
	if _, err := callVerb(t, d, "task_start", StartParams{}); err == nil {
		t.Fatal("task_start accepted an empty command")
	}
}

func TestStartRejectsTheKillOutputCap(t *testing.T) {
	d := newDaemon(t)
	_, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, OnOutputCap: "kill",
	})
	if err == nil {
		t.Fatal("task_start accepted on_output_cap=kill, which Plan 2 does not implement")
	}
}

func TestStartDefaultsToAPseudoTerminal(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "test -t 1 && echo yes || echo no"},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	e, _ := d.Reg.Get(got.(StartResult).ID)
	waitForState(t, e)
	if !e.Record().PTY {
		t.Fatal("PTY = false, want true by default")
	}
}

func TestAnOptedInCapReportsKilledNotSignaled(t *testing.T) {
	d := newDaemon(t)
	one := 1
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "sleep 60"}, KillAfterS: &one,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	e, _ := d.Reg.Get(got.(StartResult).ID)

	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: taskd asked for this kill", state)
	}
	if e.Record().Exit != nil {
		t.Fatal("Exit is set for a killed task, want nil")
	}
}

func TestStatusWithNoIDsListsEveryTask(t *testing.T) {
	d := newDaemon(t)
	for range 2 {
		if _, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "exit 0"}}); err != nil {
			t.Fatalf("task_start: %v", err)
		}
	}

	got, err := callVerb(t, d, "task_status", StatusParams{})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if n := len(got.(StatusResult).Tasks); n != 2 {
		t.Fatalf("listed %d tasks, want 2", n)
	}
}

func TestStatusByIDReportsOnlyThatTask(t *testing.T) {
	d := newDaemon(t)
	first, _ := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "exit 0"}})
	if _, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "exit 0"}}); err != nil {
		t.Fatalf("second task_start: %v", err)
	}

	id := first.(StartResult).ID
	got, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	tasks := got.(StatusResult).Tasks
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("task_status returned %+v, want only %s", tasks, id)
	}
}

func TestStatusReportsAnUnknownIDAsAnError(t *testing.T) {
	d := newDaemon(t)

	_, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{"no-such-task"}})
	if err == nil {
		t.Fatal("task_status succeeded for an id nobody owns")
	}
	if !strings.Contains(err.Error(), "no-such-task") {
		t.Fatalf("error = %q, want it to name the missing id", err)
	}
}

func TestStatusFiltersBySession(t *testing.T) {
	d := newDaemon(t)
	if _, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, Session: "s1",
	}); err != nil {
		t.Fatalf("task_start: %v", err)
	}
	if _, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, Session: "s2",
	}); err != nil {
		t.Fatalf("task_start: %v", err)
	}

	got, err := callVerb(t, d, "task_status", StatusParams{Session: "s1"})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	tasks := got.(StatusResult).Tasks
	if len(tasks) != 1 || tasks[0].Session != "s1" {
		t.Fatalf("session filter returned %+v, want only the s1 task", tasks)
	}
}
