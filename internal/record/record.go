// SPDX-License-Identifier: AGPL-3.0-or-later

// Package record stores one task's metadata on disk, next to its log.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/supervisor"
)

// FileName is the record's name inside a task directory.
const FileName = "meta.json"

// Record is everything taskd knows about one task, including after the
// process has ended and after the daemon has restarted.
type Record struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	Dir         string   `json:"cwd,omitempty"`
	PTY         bool     `json:"pty"`
	KillAfterS  *int     `json:"kill_after_s"`
	OnOutputCap string   `json:"on_output_cap"`

	Harness string `json:"harness,omitempty"`
	Session string `json:"session,omitempty"`

	State supervisor.State `json:"state"`
	PID   int              `json:"pid,omitempty"`

	// Exit is nil unless State is exited. A task that a signal ended, that
	// never started, or whose status was lost has no exit code, and a zero
	// here would read as success. Keep it a pointer so that case cannot be
	// represented.
	Exit   *int   `json:"exit_code,omitempty"`
	Signal string `json:"signal,omitempty"`

	MaxRSSBytes int64  `json:"max_rss_bytes,omitempty"`
	OutputErr   string `json:"output_err,omitempty"`

	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	// Written counts every byte the task ever produced. Retained counts the
	// bytes still on disk. They differ once rotation has discarded output.
	Written  int64 `json:"written"`
	Retained int64 `json:"retained"`
}

// Save writes r into dir atomically.
//
// It writes a temporary file in the same directory and renames it over the
// record, so a reader never sees a half-written record and a crash never
// leaves one.
func Save(dir string, r Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("record: encode %s: %w", r.ID, err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("record: create temp in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	if err := writeAndClose(tmp, b); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("record: rename into %s: %w", dir, err)
	}
	return nil
}

// writeAndClose writes b to f, gives it private permissions, flushes it, and
// closes it. It closes f on every path.
func writeAndClose(f *os.File, b []byte) (err error) {
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("record: close %s: %w", f.Name(), cerr)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("record: chmod %s: %w", f.Name(), err)
	}
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("record: write %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("record: sync %s: %w", f.Name(), err)
	}
	return nil
}

// Load reads the record in dir.
func Load(dir string) (Record, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName)) //nolint:gosec // dir is the caller-chosen task directory, not untrusted input
	if err != nil {
		return Record{}, fmt.Errorf("record: read %s: %w", dir, err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("record: decode %s: %w", dir, err)
	}
	return r, nil
}

// Scan reads every task record under root.
//
// A task directory with no record is skipped rather than reported: a task
// being created is not a corrupt one. A root with no tasks directory yet is
// not an error either.
func Scan(root string) ([]Record, error) {
	dir := paths.TasksDir(root)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("record: list %s: %w", dir, err)
	}

	var out []Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Load(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
