// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"

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
