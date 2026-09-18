// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"testing"

	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

func TestRegistryFindsATaskByIDAndByName(t *testing.T) {
	r := NewRegistry()
	e := &Entry{Rec: record.Record{ID: "abc", Name: "build", State: supervisor.StateRunning}}
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
	if err := r.Add(&Entry{Rec: record.Record{ID: "a", Name: "build", State: supervisor.StateRunning}}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := r.Add(&Entry{Rec: record.Record{ID: "b", Name: "build", State: supervisor.StateRunning}})
	if err == nil {
		t.Fatal("Add accepted a duplicate name while the first task is running")
	}
}

func TestRegistryFreesANameWhenTheTaskEnds(t *testing.T) {
	r := NewRegistry()
	first := &Entry{Rec: record.Record{ID: "a", Name: "build", State: supervisor.StateRunning}}
	if err := r.Add(first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	first.SetState(supervisor.StateExited)

	if err := r.Add(&Entry{Rec: record.Record{ID: "b", Name: "build", State: supervisor.StateRunning}}); err != nil {
		t.Fatalf("Add after the first task ended: %v", err)
	}
	got, ok := r.Get("build")
	if !ok || got.Rec.ID != "b" {
		t.Fatal("the name did not resolve to the new task")
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("the finished task is no longer reachable by id")
	}
}

func TestRegistryAnAnonymousTaskNeedsNoName(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"a", "b"} {
		if err := r.Add(&Entry{Rec: record.Record{ID: id, State: supervisor.StateRunning}}); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	if n := len(r.List()); n != 2 {
		t.Fatalf("List returned %d entries, want 2", n)
	}
}
