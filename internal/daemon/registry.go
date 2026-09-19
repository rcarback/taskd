// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/watch"
)

// Entry is one task the daemon owns.
//
// Every field is unexported and guarded by mu: a connection goroutine can
// read LiveTask or LiveStore concurrently with a reaper clearing them at
// Finish, and that is a real data race, not just a stale read, if either
// side touches the field without the lock. Record, SetState, Live,
// LiveTask, LiveStore, Dir, Done, AttachStore, AttachTask, AttachTap, Tap,
// RequestKill, Finish, and Fail are the entire surface for touching an
// Entry from outside this file.
//
// LiveTask and LiveStore are nil for a task that has ended: the daemon
// keeps the record so status and reads still answer, and drops the live
// process and its open log file.
//
// done closes when the task reaches a terminal state. Callers use Done.
// killRequested records that taskd asked for the kill, so the waiter can
// report killed rather than signaled. terminal is set by whichever of
// Finish or Fail runs first, and makes the other (or a second call to the
// same one) a no-op instead of a double close of done.
type Entry struct {
	mu    sync.Mutex
	rec   record.Record
	task  *supervisor.Task
	store *output.Store
	dir   string
	tap   *watch.Tap

	done          chan struct{}
	killRequested bool
	terminal      bool
}

// NewEntry returns an entry whose Done channel is ready to use.
func NewEntry(rec record.Record, dir string) *Entry {
	return &Entry{rec: rec, dir: dir, done: make(chan struct{})}
}

// Done closes once the task has reached a terminal state.
func (e *Entry) Done() <-chan struct{} { return e.done }

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

// AttachStore and AttachTask record the live handles. They are separate
// because the store exists before the process does: the entry joins the
// registry with its store so a status read can find it, and the task is
// set only once supervisor.Start has succeeded. Both take the same lock
// Finish uses to clear the handles.
func (e *Entry) AttachStore(store *output.Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = store
}

// AttachTask records the entry's live process. See AttachStore.
func (e *Entry) AttachTask(task *supervisor.Task) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.task = task
}

// AttachTap records the entry's output tap. It follows AttachStore: the tap
// exists before the process does, because it is the sink supervisor.Start
// writes into.
func (e *Entry) AttachTap(tap *watch.Tap) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tap = tap
}

// Tap returns the entry's output tap, or nil for a task this daemon did not
// start — one reconciled from a record left by a previous daemon.
func (e *Entry) Tap() *watch.Tap {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tap
}

// Log returns a store for reading this task's output, plus a release
// function the caller must call.
//
// A running task uses the live store, which the daemon keeps open, and the
// release is a no-op. A finished task's store is closed, so this opens the
// log read-only from the record's byte counts and the release closes it.
func (e *Entry) Log() (*output.Store, func(), error) {
	// The store, the record, and the directory come out under ONE
	// acquisition of the lock. Reading them through LiveStore, Record and
	// Dir instead would take and drop the lock three times and let Finish
	// interleave. Those accessors each take e.mu, and e.mu is not
	// reentrant, so calling one from inside this locked region would
	// deadlock.
	e.mu.Lock()
	live, rec, dir := e.store, e.rec, e.dir
	e.mu.Unlock()

	if live != nil {
		return live, func() {}, nil
	}
	s, err := output.OpenExisting(filepath.Join(dir, "out.log"), rec.Written, rec.Retained)
	if err != nil {
		return nil, nil, err
	}
	return s, func() { _ = s.Close() }, nil
}

// handles returns the task and its store together under one lock, for the
// single goroutine that waits on the task and then closes its store.
//
// Reading the fields one at a time through LiveTask and LiveStore would let
// Finish run between the two reads and hand back a mismatched pair.
func (e *Entry) handles() (*supervisor.Task, *output.Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task, e.store
}

// RequestKill records that taskd asked for this task to end, so its
// terminal state is killed rather than signaled.
func (e *Entry) RequestKill() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.killRequested = true
}

// Finish records the terminal result and closes Done exactly once. It
// clears the entry's live task and store together with the record update.
// Taking the lock once for all of it means no caller can observe a state
// where, for example, the record already reads exited but LiveTask has not
// been cleared yet.
//
// written and retained are the byte counts the caller reads from the store
// before this call clears the handle. Finish returns the record to persist.
//
// The caller reads State before ExitCode or Signal, so Finish only fills in
// whichever one res.State makes meaningful.
//
// A second call, or a call after Fail already ran, is a no-op that returns
// the record as it already stands: only the first terminal write may close
// done, and only one of Finish or Fail is ever that first write.
func (e *Entry) Finish(res supervisor.Result, written, retained int64) record.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal {
		return e.rec
	}
	e.terminal = true

	e.rec.State = res.State
	switch res.State {
	case supervisor.StateExited:
		code := res.ExitCode
		e.rec.Exit = &code
	case supervisor.StateSignaled:
		e.rec.Signal = res.Signal.String()
		if e.killRequested {
			// The spec reserves killed for a cap taskd applied or a client
			// signal. The process really did die on a signal; what makes
			// this killed rather than signaled is that taskd asked.
			e.rec.State = supervisor.StateKilled
		}
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
	e.rec.Written = written
	e.rec.Retained = retained

	// Clear both live handles under this lock, and signal Done, so a
	// connection goroutine reading through LiveTask or LiveStore can never
	// observe a state this function is still in the middle of updating.
	e.task = nil
	e.store = nil
	close(e.done)
	return e.rec
}

// Fail records that the task never started: supervisor.Start itself
// returned an error, so there is no supervisor.Result to build a record
// from the usual way. Fail gives that record the same terminal shape Finish
// gives every other outcome — a state, an EndedAt, and no live handles —
// rather than a hand-rolled one that could drift from Finish's over time.
//
// Fail shares Finish's terminal guard: whichever of the two runs first wins,
// and the other becomes a no-op. A task that never started has no live
// handles to begin with, so clearing them here is for symmetry with Finish,
// not because either could be non-nil.
func (e *Entry) Fail(ended time.Time) record.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.terminal {
		return e.rec
	}
	e.terminal = true

	e.rec.State = supervisor.StateFailed
	e.rec.EndedAt = &ended
	e.task = nil
	e.store = nil
	close(e.done)
	return e.rec
}

// Registry holds every task the daemon knows, live or finished.
//
// Lock ordering: Registry.mu is always taken before Entry.mu, never the other
// way round. Add and List hold r.mu while calling Record or Live, which take
// e.mu. Nothing may take e.mu and then reach for r.mu — Finish and Fail touch
// no registry, and they must keep it that way. The order is one-directional
// today only because nothing has needed the other direction, so it is written
// down here rather than left for the next task to discover by deadlocking.
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
