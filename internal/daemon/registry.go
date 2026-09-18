// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"fmt"
	"sort"
	"sync"

	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

// Entry is one task the daemon owns.
//
// Task and Store are nil for a task that has ended: the daemon keeps the
// record so status and reads still answer, and drops the live process and
// its open log file.
type Entry struct {
	mu    sync.Mutex
	Rec   record.Record
	Task  *supervisor.Task
	Store *output.Store
	Dir   string
}

// Record returns a copy of the entry's record.
func (e *Entry) Record() record.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Rec
}

// SetState updates the entry's state under its own lock.
func (e *Entry) SetState(s supervisor.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Rec.State = s
}

// Live reports whether the task is still running.
func (e *Entry) Live() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.Rec.State == supervisor.StateRunning
}

// Registry holds every task the daemon knows, live or finished.
type Registry struct {
	mu     sync.Mutex
	byID   map[string]*Entry
	byName map[string]*Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[string]*Entry{}, byName: map[string]*Entry{}}
}

// Add registers e.
//
// A name must be unique among live tasks. A name whose previous holder has
// ended is free to reuse, and the finished task stays reachable by its id.
func (r *Registry) Add(e *Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec := e.Record()
	if _, taken := r.byID[rec.ID]; taken {
		return fmt.Errorf("daemon: task id %s is already registered", rec.ID)
	}
	if rec.Name != "" {
		if prev, taken := r.byName[rec.Name]; taken && prev.Live() {
			return fmt.Errorf("daemon: name %q is in use by a running task", rec.Name)
		}
		r.byName[rec.Name] = e
	}
	r.byID[rec.ID] = e
	return nil
}

// Get finds a task by id, or by the name of the task that currently holds it.
func (r *Registry) Get(key string) (*Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[key]; ok {
		return e, true
	}
	e, ok := r.byName[key]
	return e, ok
}

// List returns every entry, ordered by id so output is stable.
func (r *Registry) List() []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*Entry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record().ID < out[j].Record().ID })
	return out
}
