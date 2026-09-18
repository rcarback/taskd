// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/client"
	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/watch"
)

func TestWaitReturnsWhenTheTaskExits(t *testing.T) {
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{
		IDs:   []string{id},
		Until: []watch.Condition{{Type: watch.KindExit}},
	})
	if err != nil {
		t.Fatalf("task_wait: %v", err)
	}
	w := got.(WaitResult)
	if w.ID != id {
		t.Errorf("ID = %q, want %q", w.ID, id)
	}
	if w.Fired != string(watch.KindExit) {
		t.Errorf("Fired = %q, want %q", w.Fired, watch.KindExit)
	}
	if w.State != "exited" {
		t.Errorf("State = %q, want exited", w.State)
	}
	if w.Exit == nil || *w.Exit != 0 {
		t.Errorf("Exit = %v, want 0", w.Exit)
	}
}

func TestWaitAlwaysCarriesTheLongPollWarning(t *testing.T) {
	// The warning is the product. A blocked agent is the problem this tool
	// exists to remove, so every blocking wait says so where the model
	// reads it.
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{id}, Deliver: "block"})
	if err != nil {
		t.Fatalf("task_wait: %v", err)
	}
	if w := got.(WaitResult); !strings.Contains(w.Warning, "LONG-POLL") {
		t.Errorf("Warning = %q, want the long poll warning", w.Warning)
	}
}

func TestWaitNotifyDegradesRatherThanFails(t *testing.T) {
	// A failure would teach the agent to stop asking for notification.
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{id}, Deliver: "notify"})
	if err != nil {
		t.Fatalf("task_wait with deliver notify: %v, want a degraded success", err)
	}
	w := got.(WaitResult)
	if !strings.Contains(w.Warning, "Notification delivery unavailable") {
		t.Errorf("Warning = %q, want it to say notification was unavailable", w.Warning)
	}
	if w.Fired != string(watch.KindExit) {
		t.Errorf("Fired = %q, want the wait to have completed anyway", w.Fired)
	}
}

func TestWaitDefaultsToExitAndIdle(t *testing.T) {
	d := newDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_wait with no until: %v", err)
	}
	if w := got.(WaitResult); w.Fired != string(watch.KindExit) {
		t.Errorf("Fired = %q, want exit from the default condition set", w.Fired)
	}
}

func TestWaitRejectsAnUnknownID(t *testing.T) {
	d := newDaemon(t)
	_, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{"nosuchtask"}})
	if err == nil {
		t.Fatal("task_wait accepted an unknown id")
	}
	if !strings.Contains(err.Error(), "nosuchtask") {
		t.Errorf("error = %q, want it to name the unknown id", err)
	}
}

func TestWaitRejectsNoIDs(t *testing.T) {
	d := newDaemon(t)
	_, err := callVerb(t, d, "task_wait", WaitParams{})
	if err == nil {
		t.Fatal("task_wait with no ids returned nil, want an error")
	}
}

// TestWaitRejectsATaskWithNoTap covers the entry-with-no-tap rejection.
// Nothing in this plan yet loads a reconciled record into the registry, so
// there is no way to reach this path through the public verb API; this
// builds the same shape by hand — an Entry the registry knows about, with
// no tap ever attached — to stand in for it.
func TestWaitRejectsATaskWithNoTap(t *testing.T) {
	d := newDaemon(t)

	code := 0
	e := NewEntry(record.Record{
		ID: "reconciled", State: supervisor.StateExited, Exit: &code,
	}, t.TempDir())
	if err := d.Reg.Add(e); err != nil {
		t.Fatalf("Reg.Add: %v", err)
	}

	_, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{"reconciled"}})
	if err == nil {
		t.Fatal("task_wait accepted a task with no tap")
	}
	if !strings.Contains(err.Error(), "reconciled") {
		t.Errorf("error = %q, want it to name the task", err)
	}
	if !strings.Contains(err.Error(), "task_status") {
		t.Errorf("error = %q, want it to point at task_status", err)
	}
}

// TestServeConnReturnsWhenTheClientHangsUpDuringALongPoll covers the
// disconnect detection serveConn relies on to release a blocked task_wait:
// without it, a client that dies mid-long-poll would hold its connection
// goroutine and the wait's tap registrations for as long as the task it
// asked about keeps running.
//
// This drives serveConn directly over a real socket pair from d's own
// listener, rather than through Serve's accept loop, so the assertion is
// exactly what serveConn — the function that owns the disconnect detection
// — does, with nothing about the accept loop's own bookkeeping in the way.
func TestServeConnReturnsWhenTheClientHangsUpDuringALongPoll(t *testing.T) {
	d := newDaemon(t)

	res, err := callVerb(t, d, "task_start", StartParams{Command: "sleep", Args: []string{"30"}})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID
	e, ok := d.Reg.Get(id)
	if !ok {
		t.Fatal("the started task is not in the registry")
	}
	t.Cleanup(func() {
		if task := e.LiveTask(); task != nil {
			_ = task.Signal(syscall.SIGKILL)
		}
	})

	clientConn, err := net.Dial("unix", d.Addr())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	serverConn, err := d.ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// A condition that cannot fire inside this test's lifetime, so the
	// only way serveConn returns is by noticing the hang-up below rather
	// than by the wait completing on its own.
	b, err := json.Marshal(WaitParams{
		IDs: []string{id}, Until: []watch.Condition{{Type: watch.KindIdle, Seconds: 9999}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := proto.WriteMessage(clientConn, proto.Request{Verb: proto.VerbWait, Params: b}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	// The request is already handed to the kernel by the write above, so
	// the daemon still decodes it whether or not the client is still
	// around by the time it does — only the reply is now going nowhere,
	// exactly like a client that gave up or crashed mid-long-poll.
	if err := clientConn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		d.serveConn(t.Context(), serverConn)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveConn did not return within 5s of the client hanging up mid task_wait")
	}
}

// TestClientCallStartsATaskAndWaitsOnItOverTheSocket drives a real daemon
// through the client package rather than a handler or a hand-rolled
// connection, exercising task_start and task_wait exactly as the taskd wait
// subcommand does.
func TestClientCallStartsATaskAndWaitsOnItOverTheSocket(t *testing.T) {
	root := shortRoot(t)
	d, err := New(root, clock.System())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Register()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v, want nil after cancellation", err)
		}
	})

	startParams, err := json.Marshal(StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	startRes, err := client.Call(root, proto.Request{Verb: proto.VerbStart, Params: startParams})
	if err != nil {
		t.Fatalf("client.Call task_start: %v", err)
	}
	if !startRes.OK {
		t.Fatalf("task_start OK = false, error = %q", startRes.Error)
	}
	var started StartResult
	if err := json.Unmarshal(startRes.Result, &started); err != nil {
		t.Fatalf("Unmarshal StartResult: %v", err)
	}

	waitParams, err := json.Marshal(WaitParams{
		IDs: []string{started.ID}, Until: []watch.Condition{{Type: watch.KindExit}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	waitRes, err := client.Call(root, proto.Request{Verb: proto.VerbWait, Params: waitParams})
	if err != nil {
		t.Fatalf("client.Call task_wait: %v", err)
	}
	if !waitRes.OK {
		t.Fatalf("task_wait OK = false, error = %q", waitRes.Error)
	}
	var w WaitResult
	if err := json.Unmarshal(waitRes.Result, &w); err != nil {
		t.Fatalf("Unmarshal WaitResult: %v", err)
	}
	if w.ID != started.ID {
		t.Errorf("ID = %q, want %q", w.ID, started.ID)
	}
	if w.State != "exited" {
		t.Errorf("State = %q, want exited", w.State)
	}
}
