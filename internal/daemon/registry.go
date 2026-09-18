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
// Every field is unexported and guarded by mu: a connection goroutine can
// read LiveTask or LiveStore concurrently with a reaper clearing them at
// Finish, and that is a real data race, not just a stale read, if either
// side touches the field without the lock. Record, SetState, Live,
// LiveTask, LiveStore, Dir, and Finish are the entire surface for touching
// an Entry from outside this file.
//
// LiveTask and LiveStore are nil for a task that has ended: the daemon
// keeps the record so status and reads still answer, and drops the live
// process and its open log file.
type Entry struct {
	mu    sync.Mutex
	rec   record.Record
	task  *supervisor.Task
	store *output.Store
	dir   string
}

// Record returns a copy of the entry's record.
func (e *Entry) Record() record.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rec
}

// SetState updates the entry's state under its own lock.
func (e *Entry) SetState(s supervisor.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.State = s
}

// Live reports whether the task is still running.
func (e *Entry) Live() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rec.State == supervisor.StateRunning
}

// LiveTask returns the entry's running process, or nil once the task has
// ended.
func (e *Entry) LiveTask() *supervisor.Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task
}

// LiveStore returns the entry's open output log, or nil once the task has
// ended.
func (e *Entry) LiveStore() *output.Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store
}

// Dir returns the task's directory on disk.
func (e *Entry) Dir() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dir
}

// Finish records that the task has ended, filling in the record's terminal
// fields from res, and clears the entry's live task and store together with
// them. Taking the lock once for all of it means no caller can observe a
// state where, for example, the record already reads exited but LiveTask
// has not been cleared yet.
//
// The caller reads State before ExitCode or Signal, so Finish only fills in
// whichever one res.State makes meaningful.
func (e *Entry) Finish(res supervisor.Result) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.rec.State = res.State
	switch res.State {
	case supervisor.StateExited:
		code := res.ExitCode
		e.rec.Exit = &code
	case supervisor.StateSignaled:
		e.rec.Signal = res.Signal.String()
	case supervisor.StateRunning, supervisor.StateKilled, supervisor.StateFailed, supervisor.StateLost:
		// Neither field applies: StateRunning is not terminal, and the
		// other three carry no exit code or signal of their own.
	}
	ended := res.Ended
	e.rec.EndedAt = &ended
	if res.OutputErr != nil {
		e.rec.OutputErr = res.OutputErr.Error()
	}
	e.rec.MaxRSSBytes = res.MaxRSSBytes

	e.task = nil
	e.store = nil
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
