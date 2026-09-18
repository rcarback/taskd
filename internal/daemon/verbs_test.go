// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
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
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)
	waitForState(t, e)
	if !e.Record().PTY {
		t.Fatal("PTY = false, want true by default")
	}

	// The record field alone proves nothing: start copies it straight from
	// the request, so setting Spec.PTY false while leaving the record alone
	// would keep that assertion green. The task's own answer to "test -t 1"
	// is what shows it really got a terminal.
	res, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if data := res.(ReadResult).Data; !strings.Contains(data, "yes") {
		t.Fatalf("Data = %q, want yes: the task did not see a terminal on stdout", data)
	}
}

// TestAnOptedInCapReportsKilledNotSignaled uses clock.Fake, not
// clock.System, so the cap fires the instant the test advances the clock
// rather than after a real one-second wait: enforceCap's own kill signal
// still hits a real child process, but the trigger for it is deterministic.
func TestAnOptedInCapReportsKilledNotSignaled(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	d, err := New(shortRoot(t), clk)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Register()

	one := 1
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "sleep 60"}, KillAfterS: &one,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	e, _ := d.Reg.Get(got.(StartResult).ID)

	// enforceCap's goroutine registers its timer with the fake clock by
	// calling After; BlockUntil waits for that registration so Advance
	// below can never race ahead of a timer that has not been set up yet.
	clk.BlockUntil(1)
	clk.Advance(time.Second)

	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: taskd asked for this kill", state)
	}
	if e.Record().Exit != nil {
		t.Fatal("Exit is set for a killed task, want nil")
	}
}

func TestStartRejectsANonPositiveKillAfterS(t *testing.T) {
	d := newDaemon(t)
	for _, seconds := range []int{0, -1} {
		_, err := callVerb(t, d, "task_start", StartParams{
			Command: "sh", Args: []string{"-c", "exit 0"}, KillAfterS: &seconds,
		})
		if err == nil {
			t.Fatalf("task_start accepted kill_after_s = %d, want an error", seconds)
		}
	}
}

func TestStartRecordsAFailedLaunchAsATerminalFailure(t *testing.T) {
	d := newDaemon(t)

	_, err := callVerb(t, d, "task_start", StartParams{
		Command: "definitely-not-a-real-command-xyz", Name: "willfail",
	})
	if err == nil {
		t.Fatal("task_start succeeded for a command that cannot launch")
	}

	e, ok := d.Reg.Get("willfail")
	if !ok {
		t.Fatal("the entry for the failed launch is not in the registry")
	}
	select {
	case <-e.Done():
	default:
		t.Fatal("Done() has not closed for a task that failed to launch")
	}

	rec := e.Record()
	if rec.State != supervisor.StateFailed {
		t.Fatalf("State = %q, want failed", rec.State)
	}
	if rec.EndedAt == nil {
		t.Fatal("EndedAt is nil for a failed launch, want it set like every other terminal record")
	}
	if e.LiveTask() != nil {
		t.Fatal("LiveTask() is non-nil after a failed launch")
	}
	if e.LiveStore() != nil {
		t.Fatal("LiveStore() is non-nil after a failed launch: a read verb would reach a closed store")
	}

	// A failed launch must not permanently burn the name: the slot is free
	// again for a task that can actually run.
	if _, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, Name: "willfail",
	}); err != nil {
		t.Fatalf("task_start after the failed launch: %v", err)
	}
}

// TestStartKeepsATaskAliveWhenTheInitialSaveFails injects a failure into
// start's post-launch record.Save by substituting the package-level
// saveRecord variable, since a real filesystem failure at exactly that
// point cannot be produced deterministically (the process is already
// running by the time that save happens, with no hook between the two). The
// substitution is read only synchronously inside this call to task_start,
// never by a background goroutine, so restoring it once callVerb returns
// cannot race a concurrent reader.
func TestStartKeepsATaskAliveWhenTheInitialSaveFails(t *testing.T) {
	d := newDaemon(t)

	orig := saveRecord
	saveRecord = func(string, record.Record) error {
		return errors.New("boom: injected save failure")
	}
	t.Cleanup(func() { saveRecord = orig })

	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "sleep 30"}, Name: "job",
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	res := got.(StartResult)

	e, ok := d.Reg.Get(res.ID)
	if !ok {
		t.Fatal("the started task is not in the registry despite the save failure")
	}
	if !e.Live() {
		t.Fatal("Live() = false right after task_start, want true: a save failure must not fail a task that already started")
	}
	if e.LiveTask() == nil {
		t.Fatal("LiveTask() is nil right after task_start, want the running process")
	}

	// The name stays taken while the task is live, proving the entry was
	// not rolled back or abandoned because of the save failure.
	if _, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, Name: "job",
	}); err == nil {
		t.Fatal("task_start accepted a duplicate name for a task the save failure should not have removed")
	}
}

func TestStatusWithNoMatchesEncodesAnEmptyArrayNotNull(t *testing.T) {
	d := newDaemon(t)

	got, err := callVerb(t, d, "task_status", StatusParams{Session: "no-such-session"})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	res := got.(StatusResult)
	if len(res.Tasks) != 0 {
		t.Fatalf("Tasks = %+v, want empty", res.Tasks)
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"tasks":[]`) {
		t.Fatalf("encoded = %s, want \"tasks\":[] rather than null", b)
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
