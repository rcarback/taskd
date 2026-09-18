// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

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
}

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
		e.SetState(supervisor.StateFailed)
		_ = record.Save(dir, e.Record())
		return StartResult{}, err
	}
	e.AttachTask(task)

	if err := record.Save(dir, rec); err != nil {
		return StartResult{}, err
	}

	go d.await(e)
	if p.KillAfterS != nil {
		go d.enforceCap(e, *p.KillAfterS)
	}
	return StartResult{ID: id, Name: p.Name}, nil
}

// await waits for the task to end, then records the outcome.
func (d *Daemon) await(e *Entry) {
	task, store := e.handles()
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

	var out []StatusEntry
	for _, e := range d.Reg.List() {
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
