// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/rcarback/taskd/internal/ansi"
	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/taskdir"
)

// Register installs every verb handler this plan implements.
func (d *Daemon) Register() {
	d.Handle(proto.VerbStart, jsonHandler(d.start))
	d.Handle(proto.VerbStatus, jsonHandler(d.status))
	d.Handle(proto.VerbRead, jsonHandler(d.read))
	d.Handle(proto.VerbSearch, jsonHandler(d.search))
}

// saveRecord persists a task's record to disk. It is a variable, not a
// direct call to record.Save, only so a test can inject a failure on the
// post-launch path in start below without depending on real filesystem
// timing: that path's own record.Save is best-effort, and the only way to
// exercise its failure deterministically is to substitute the function.
// await's own save, on the task's eventual real completion, always calls
// record.Save directly and is never affected by this.
//
// Because a test swaps this variable and restores it, no test in this
// package may call t.Parallel() while the seam exists. Two parallel tests
// mutating it would race, and the symptom — one test's task_start silently
// observing another test's injected failure — reads as a flaky assertion
// rather than as the data race it is.
var saveRecord = record.Save

// jsonHandler adapts a typed handler into a Handler by decoding its params.
func jsonHandler[P any, R any](fn func(P) (R, error)) Handler {
	return func(raw json.RawMessage) (any, error) {
		var p P
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("daemon: decode params: %w", err)
			}
		}
		return fn(p)
	}
}

// start spawns a task and returns its id.
func (d *Daemon) start(p StartParams) (StartResult, error) {
	if p.Command == "" {
		return StartResult{}, fmt.Errorf("daemon: task_start needs a command")
	}
	if p.KillAfterS != nil && *p.KillAfterS <= 0 {
		// nil already means "no cap" (see StartParams.KillAfterS), so 0 is
		// not a second spelling of the same thing: it is the caller
		// explicitly asking for a cap and then giving it a non-duration. A
		// zero or negative cap would otherwise fire on its very next tick
		// and kill a task the caller did not knowingly opt into killing
		// immediately.
		return StartResult{}, fmt.Errorf("daemon: kill_after_s must be positive, got %d", *p.KillAfterS)
	}
	outputCap := p.OnOutputCap
	if outputCap == "" {
		outputCap = defaultOnOutputCap
	}
	if outputCap != defaultOnOutputCap {
		return StartResult{}, fmt.Errorf(
			"daemon: on_output_cap %q is not implemented yet; only %q is. A killing cap needs the overflow signal that arrives with pattern support",
			outputCap, defaultOnOutputCap)
	}

	dir, id, err := taskdir.New(d.Root)
	if err != nil {
		return StartResult{}, err
	}

	maxOutput := p.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	store, err := output.Open(filepath.Join(dir, "out.log"), maxOutput)
	if err != nil {
		return StartResult{}, err
	}

	usePTY := p.PTY == nil || *p.PTY
	rec := record.Record{
		ID: id, Name: p.Name,
		Command: p.Command, Args: p.Args, Dir: p.Cwd,
		PTY: usePTY, KillAfterS: p.KillAfterS, OnOutputCap: outputCap,
		Harness: p.Harness, Session: p.Session,
		State: supervisor.StateRunning, StartedAt: d.Clk.Now(),
	}

	e := NewEntry(rec, dir)
	e.AttachStore(store)
	if err := d.Reg.Add(e); err != nil {
		_ = store.Close()
		return StartResult{}, err
	}

	task, err := supervisor.Start(supervisor.Spec{
		Command: p.Command, Args: p.Args, Dir: p.Cwd,
		Env: os.Environ(), PTY: usePTY,
	}, storeWriter{store}, d.Clk)
	if err != nil {
		_ = store.Close()
		// Fail gives this record the same terminal shape Finish gives every
		// other outcome (a state, an EndedAt, no live handles) and closes
		// Done, so the entry a failed launch leaves behind is not
		// distinguishable in shape from one a real task produced, and
		// nothing waiting on Done blocks forever.
		failed := e.Fail(d.Clk.Now())
		_ = saveRecord(dir, failed)
		return StartResult{}, err
	}
	e.AttachTask(task)

	// Best-effort: the process is already running at this point, so a
	// failure to persist its initial metadata must not stop it from being
	// waited on and reaped below. Returning an error here instead would
	// leave nobody calling task.Wait(), wedge the entry as running forever,
	// and permanently burn its name in the registry over a write failure
	// that has nothing to do with the task itself. await's own save, when
	// the task ends, persists the record either way, so a caller reading
	// task_status still sees accurate state even if this particular write
	// failed.
	_ = saveRecord(dir, rec)

	go d.await(e)
	if p.KillAfterS != nil {
		go d.enforceCap(e, *p.KillAfterS)
	}
	return StartResult{ID: id, Name: p.Name}, nil
}

// await waits for the task to end, then records the outcome.
func (d *Daemon) await(e *Entry) {
	task, store := e.handles()
	if task == nil || store == nil {
		// Finish or Fail already ran for this entry — start attaches both
		// before launching this goroutine, so this guards the invariant
		// locally rather than relying on that call order never changing.
		return
	}
	res := task.Wait()
	written, retained := store.Counts()
	_ = store.Close()

	rec := e.Finish(res, written, retained)
	_ = record.Save(e.Dir(), rec)
}

// enforceCap ends the task after seconds, if it is still running.
//
// This is the only path in Plan 2 that ends a task without a client asking,
// and it runs only when the caller set kill_after_s. Nothing creates a cap
// by default.
func (d *Daemon) enforceCap(e *Entry, seconds int) {
	select {
	case <-e.Done():
	case <-d.Clk.After(time.Duration(seconds) * time.Second):
		task := e.LiveTask()
		if task == nil {
			return // it ended while the timer was firing
		}
		e.RequestKill()
		_ = task.Signal(syscall.SIGKILL)
	}
}

// status reports terse state for the requested tasks, or lists.
func (d *Daemon) status(p StatusParams) (StatusResult, error) {
	if len(p.IDs) > 0 {
		out := make([]StatusEntry, 0, len(p.IDs))
		for _, id := range p.IDs {
			e, ok := d.Reg.Get(id)
			if !ok {
				return StatusResult{}, fmt.Errorf("daemon: no task %q", id)
			}
			out = append(out, statusOf(e))
		}
		return StatusResult{Tasks: out}, nil
	}

	entries := d.Reg.List()
	// A non-nil, possibly-empty slice: Go's zero value marshals to JSON
	// null, and a caller iterating "tasks" wants [] when nothing matched,
	// not null.
	out := make([]StatusEntry, 0, len(entries))
	for _, e := range entries {
		rec := e.Record()
		if !p.All && p.Session != "" && rec.Session != p.Session {
			continue
		}
		out = append(out, statusOf(e))
	}
	return StatusResult{Tasks: out}, nil
}

// statusOf converts an entry to its terse form.
func statusOf(e *Entry) StatusEntry {
	r := e.Record()
	return StatusEntry{
		ID: r.ID, Name: r.Name, State: string(r.State), Command: r.Command,
		Exit: r.Exit, Signal: r.Signal, Session: r.Session,
		Written: r.Written, Retained: r.Retained,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, OutputErr: r.OutputErr,
	}
}

// read returns output by cursor, or the last N lines.
func (d *Daemon) read(p ReadParams) (ReadResult, error) {
	if p.Since != nil && p.Tail != nil {
		return ReadResult{}, fmt.Errorf("daemon: task_read takes since or tail, not both")
	}
	e, ok := d.Reg.Get(p.ID)
	if !ok {
		return ReadResult{}, fmt.Errorf("daemon: no task %q", p.ID)
	}
	store, release, err := e.Log()
	if err != nil {
		return ReadResult{}, err
	}
	defer release()

	if p.Tail != nil {
		raw, err := store.Tail(*p.Tail)
		if err != nil {
			return ReadResult{}, err
		}
		clean, _ := ansi.Strip(raw)
		written, retained := store.Counts()
		// Next always jumps to the end of the stream, even for a task still
		// running: a tail read is a snapshot of the last N lines, not a
		// cursor into the middle of one, so there is no meaningful earlier
		// offset to resume from. A caller who follows a tail read with a
		// since read gets nothing between the tail window and this call.
		return ReadResult{
			Data: string(clean), Next: written,
			TruncatedBytes: written - retained, EOF: !e.Live(),
		}, nil
	}

	cursor := int64(0)
	if p.Since != nil {
		cursor = *p.Since
	}
	limit := p.MaxBytes
	if limit <= 0 {
		limit = defaultReadBytes
	}

	raw, next, truncated, err := store.ReadSince(cursor, limit)
	if err != nil {
		return ReadResult{}, err
	}
	clean, pending := ansi.Strip(raw)
	next = rewind(next, cursor, pending, e.Live())

	return ReadResult{
		Data: string(clean), Next: next, TruncatedBytes: truncated,
		EOF: !e.Live() && next >= store.Written(),
	}, nil
}

// rewind pulls next back so an escape sequence cut by the read boundary is
// re-read whole on the next call.
//
// A rewind that makes no progress would spin, so a running task simply
// returns the caller's own cursor and waits for more output. A finished task
// has no more output, so the unfinished sequence is dropped rather than held
// forever, which would make EOF unreachable.
func rewind(next, cursor int64, pending int, live bool) int64 {
	if pending == 0 {
		return next
	}
	rewound := next - int64(pending)
	if rewound > cursor {
		return rewound
	}
	if live {
		return cursor
	}
	return next
}

// search runs a regular expression over the retained log.
func (d *Daemon) search(p SearchParams) (SearchResult, error) {
	re, err := regexp.Compile(p.Regex)
	if err != nil {
		return SearchResult{}, fmt.Errorf("daemon: task_search regex: %w", err)
	}
	e, ok := d.Reg.Get(p.ID)
	if !ok {
		return SearchResult{}, fmt.Errorf("daemon: no task %q", p.ID)
	}
	store, release, err := e.Log()
	if err != nil {
		return SearchResult{}, err
	}
	defer release()

	// ReadSince clamps a cursor below base up to base and reports what
	// rotation discarded, so starting at 0 reads the whole retained log
	// without reading Written and Retained as two separate, racing counters.
	_, retained := store.Counts()
	// retained is an int64 bounded only by the task's max_output. Converting
	// it straight to int would wrap negative on a 32-bit build once
	// max_output passes 2 GiB, so clamp to the platform's int range first.
	limit := retained + 1
	if limit <= 0 || limit > math.MaxInt {
		limit = math.MaxInt
	}
	raw, _, truncated, err := store.ReadSince(0, int(limit))
	if err != nil {
		return SearchResult{}, err
	}
	clean, _ := ansi.Strip(raw)
	var lines []string
	if len(clean) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(clean), "\n"), "\n")
	}

	maxMatches := p.MaxMatches
	if maxMatches <= 0 {
		maxMatches = defaultMaxMatches
	}

	// A non-nil, possibly-empty slice: Go's zero value marshals to JSON
	// null, and a caller iterating "matches" wants [] when nothing matched,
	// not null.
	out := make([]Match, 0, maxMatches)
	more := false
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		if len(out) == maxMatches {
			more = true
			break
		}
		out = append(out, Match{
			LineNumber: i + 1, Line: line,
			Before: window(lines, i-p.Context, i),
			After:  window(lines, i+1, i+1+p.Context),
		})
	}
	return SearchResult{Matches: out, TruncatedBytes: truncated, More: more}, nil
}

// window returns lines[lo:hi], clamped to the slice.
//
// The result is a non-nil, possibly-empty slice: Match.Before and
// Match.After carry no omitempty tag, so a caller iterating "before" or
// "after" gets [] rather than a missing key when the window is empty, which
// at the default context of 0 is every match.
func window(lines []string, lo, hi int) []string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	if lo >= hi {
		return []string{}
	}
	return append([]string{}, lines[lo:hi]...)
}

// storeWriter adapts an output.Store to io.Writer.
//
// Exactly one storeWriter value is built per task and passed once, so
// os/exec collapses stdout and stderr onto one copy goroutine.
type storeWriter struct{ s *output.Store }

func (w storeWriter) Write(p []byte) (int, error) {
	if err := w.s.Append(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
