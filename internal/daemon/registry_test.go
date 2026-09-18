// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

func TestRegistryFindsATaskByIDAndByName(t *testing.T) {
	r := NewRegistry()
	e := NewEntry(record.Record{ID: "abc", Name: "build", State: supervisor.StateRunning}, "")
	if err := r.Add(e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, key := range []string{"abc", "build"} {
		if got, ok := r.Get(key); !ok || got != e {
			t.Fatalf("Get(%q) did not return the entry", key)
		}
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get returned an entry for an unknown key")
	}
}

func TestRegistryRejectsADuplicateLiveName(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(NewEntry(record.Record{ID: "a", Name: "build", State: supervisor.StateRunning}, "")); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := r.Add(NewEntry(record.Record{ID: "b", Name: "build", State: supervisor.StateRunning}, ""))
	if err == nil {
		t.Fatal("Add accepted a duplicate name while the first task is running")
	}
}

func TestRegistryFreesANameWhenTheTaskEnds(t *testing.T) {
	r := NewRegistry()
	first := NewEntry(record.Record{ID: "a", Name: "build", State: supervisor.StateRunning}, "")
	if err := r.Add(first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	first.SetState(supervisor.StateExited)

	if err := r.Add(NewEntry(record.Record{ID: "b", Name: "build", State: supervisor.StateRunning}, "")); err != nil {
		t.Fatalf("Add after the first task ended: %v", err)
	}
	got, ok := r.Get("build")
	if !ok || got.Record().ID != "b" {
		t.Fatal("the name did not resolve to the new task")
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("the finished task is no longer reachable by id")
	}
}

func TestRegistryAnAnonymousTaskNeedsNoName(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"a", "b"} {
		if err := r.Add(NewEntry(record.Record{ID: id, State: supervisor.StateRunning}, "")); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	if n := len(r.List()); n != 2 {
		t.Fatalf("List returned %d entries, want 2", n)
	}
}

func TestEntryFinishRecordsTerminalStateAndClearsLiveHandles(t *testing.T) {
	cases := []struct {
		name string
		res  supervisor.Result
	}{
		{"exited", supervisor.Result{State: supervisor.StateExited, ExitCode: 7, Ended: time.Unix(100, 0)}},
		{"signaled", supervisor.Result{State: supervisor.StateSignaled, Signal: syscall.SIGKILL, Ended: time.Unix(200, 0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEntry(record.Record{ID: "a", State: supervisor.StateRunning}, "")
			e.AttachTask(&supervisor.Task{})
			e.AttachStore(&output.Store{})

			rec := e.Finish(tc.res, 11, 5)

			if e.LiveTask() != nil {
				t.Fatal("LiveTask() is non-nil after Finish")
			}
			if e.LiveStore() != nil {
				t.Fatal("LiveStore() is non-nil after Finish")
			}
			select {
			case <-e.Done():
			default:
				t.Fatal("Done() has not closed after Finish")
			}

			if rec.State != tc.res.State {
				t.Fatalf("State = %q, want %q", rec.State, tc.res.State)
			}
			if got := e.Record(); !reflect.DeepEqual(got, rec) {
				t.Fatalf("Record() = %+v, want the record Finish returned %+v", got, rec)
			}
			if rec.EndedAt == nil || !rec.EndedAt.Equal(tc.res.Ended) {
				t.Fatalf("EndedAt = %v, want %v", rec.EndedAt, tc.res.Ended)
			}
			if rec.Written != 11 || rec.Retained != 5 {
				t.Fatalf("Written, Retained = %d, %d, want 11, 5", rec.Written, rec.Retained)
			}
			switch tc.res.State {
			case supervisor.StateExited:
				if rec.Exit == nil || *rec.Exit != tc.res.ExitCode {
					t.Fatalf("Exit = %v, want %d", rec.Exit, tc.res.ExitCode)
				}
				if rec.Signal != "" {
					t.Fatalf("Signal = %q, want empty for an exited task", rec.Signal)
				}
			case supervisor.StateSignaled:
				if rec.Signal != tc.res.Signal.String() {
					t.Fatalf("Signal = %q, want %q", rec.Signal, tc.res.Signal.String())
				}
				if rec.Exit != nil {
					t.Fatalf("Exit = %v, want nil for a signaled task", rec.Exit)
				}
			}
		})
	}
}

func TestEntryFinishRemapsAKillRequestedSignalToKilled(t *testing.T) {
	e := NewEntry(record.Record{ID: "a", State: supervisor.StateRunning}, "")
	e.RequestKill()

	rec := e.Finish(supervisor.Result{State: supervisor.StateSignaled, Signal: syscall.SIGKILL}, 0, 0)

	if rec.State != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: taskd asked for this kill", rec.State)
	}
	if rec.Signal != syscall.SIGKILL.String() {
		t.Fatalf("Signal = %q, want %q: the process really did die on a signal", rec.Signal, syscall.SIGKILL.String())
	}
	if rec.Exit != nil {
		t.Fatalf("Exit = %v, want nil for a killed task", rec.Exit)
	}
}
