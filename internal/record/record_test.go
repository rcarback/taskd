// SPDX-License-Identifier: AGPL-3.0-or-later

package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/supervisor"
)

func exited(code int) *int { return &code }

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Record{
		ID:      "abc",
		Name:    "build",
		Command: "cargo",
		Args:    []string{"build"},
		Dir:     "/repo",
		PTY:     true,
		State:   supervisor.StateExited,
		Exit:    exited(0),
		Written: 42,
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name || got.State != want.State {
		t.Fatalf("Load returned %+v, want the saved record", got)
	}
	if got.Exit == nil || *got.Exit != 0 {
		t.Fatalf("Exit = %v, want a pointer to 0", got.Exit)
	}
}

func TestASignaledRecordCarriesNoExitCode(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{ID: "a", State: supervisor.StateSignaled, Signal: "SIGKILL"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Exit != nil {
		t.Fatalf("Exit = %d, want nil: a signaled task has no exit code", *got.Exit)
	}
	if got.Signal != "SIGKILL" {
		t.Fatalf("Signal = %q, want SIGKILL", got.Signal)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{ID: "a", State: supervisor.StateRunning}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := Save(dir, Record{ID: "a", State: supervisor.StateExited, Exit: exited(3)}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("directory holds %d entries, want only %s: a temp file leaked", len(entries), FileName)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != supervisor.StateExited {
		t.Fatalf("State = %q, want the second write to have replaced the first", got.State)
	}
}

func TestSaveWritesAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{ID: "a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

func TestScanReturnsEveryTaskRecord(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"a", "b"} {
		dir := filepath.Join(root, "tasks", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := Save(dir, Record{ID: id, State: supervisor.StateRunning, StartedAt: time.Unix(0, 0)}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Scan returned %d records, want 2", len(got))
	}
}

func TestScanSkipsADirectoryWithNoRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks", "half-made"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d records, want 0: a directory with no record is not a task", len(got))
	}
}

func TestScanOnAMissingTasksDirectoryIsNotAnError(t *testing.T) {
	got, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan on a fresh root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d records, want 0", len(got))
	}
}
