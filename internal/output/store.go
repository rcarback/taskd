// SPDX-License-Identifier: AGPL-3.0-or-later

// Package output stores task output in a bounded, append-only log and serves
// reads by absolute stream offset.
package output

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
)

// file is the subset of *os.File that Store uses. Tests substitute a fake
// to exercise a partial write, which a real file cannot reliably reproduce
// on demand (it needs conditions such as ENOSPC, EFBIG, or EIO).
type file interface {
	io.WriterAt
	io.ReaderAt
	Write(p []byte) (int, error)
	Truncate(size int64) error
	Seek(offset int64, whence int) (int64, error)
	Stat() (os.FileInfo, error)
	Close() error
}

// Store holds the output of one task.
//
// Callers address the log by absolute stream offset. Offsets keep counting
// across rotation, so a cursor stays meaningful after old bytes are dropped.
type Store struct {
	mu       sync.Mutex
	path     string
	file     file
	maxBytes int64
	written  int64 // total bytes ever appended
	base     int64 // stream offset of the first byte still on disk
	readOnly bool
}

// Open creates or truncates the log at path. The file never exceeds maxBytes.
func Open(path string, maxBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("output: maxBytes must be positive, got %d", maxBytes)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600) //nolint:gosec // path is the caller-chosen task output location, not untrusted input
	if err != nil {
		return nil, fmt.Errorf("output: open %s: %w", path, err)
	}
	return &Store{path: path, file: f, maxBytes: maxBytes}, nil
}

// OpenExisting opens a finished task's log for reading only.
//
// written and retained come from the task's record. The file holds exactly
// the last retained bytes of the stream, so the first byte on disk is at
// stream offset written-retained and every cursor keeps its meaning.
func OpenExisting(path string, written, retained int64) (*Store, error) {
	if retained < 0 || written < retained {
		return nil, fmt.Errorf("output: written=%d retained=%d is not a valid log", written, retained)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // the caller's own task log
	if err != nil {
		return nil, fmt.Errorf("output: open %s: %w", path, err)
	}
	// maxBytes is not a cap here: the readOnly guard in Append means rotate
	// can never run against this Store, so the field is otherwise unused.
	// retained+1 exists only to keep maxBytes positive when retained is 0 (a
	// task with no output), satisfying the precondition Open enforces on
	// every other Store. Do not read this field expecting the task's real
	// max_output — record.Record does not persist that value, so a reopened
	// log has no way to recover it.
	return &Store{
		path: path, file: f, maxBytes: retained + 1,
		written: written, base: written - retained, readOnly: true,
	}, nil
}

// Append writes p to the log, then rotates it if the write pushed the log
// past its cap. The log can transiently exceed maxBytes between the write
// and the rotation, but never stays over it once Append returns.
func (s *Store) Append(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.readOnly {
		return fmt.Errorf("output: %s is open for reading only", s.path)
	}

	// Advance written by what the file actually accepted, even on error: a
	// partial write (possible on ENOSPC, EFBIG, or EIO) still landed those
	// bytes on disk, and written must keep tracking the file's real length
	// so a later ReadSince keeps reading the stream at the right offset.
	n, err := s.file.Write(p)
	s.written += int64(n)
	if err != nil {
		return fmt.Errorf("output: write to %s: %w", s.path, err)
	}

	if s.written-s.base > s.maxBytes {
		if err := s.rotate(); err != nil {
			return err
		}
	}
	return nil
}

// rotate discards the oldest half of the retained log. The caller holds s.mu.
func (s *Store) rotate() error {
	keep := s.maxBytes / 2
	start := s.written - keep
	if start < s.base {
		start = s.base
	}

	buf := make([]byte, s.written-start)
	if _, err := s.file.ReadAt(buf, start-s.base); err != nil {
		return fmt.Errorf("output: read for rotation: %w", err)
	}
	if err := s.file.Truncate(0); err != nil {
		return fmt.Errorf("output: truncate: %w", err)
	}
	if _, err := s.file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("output: rewrite after rotation: %w", err)
	}
	if _, err := s.file.Seek(int64(len(buf)), 0); err != nil {
		return fmt.Errorf("output: seek after rotation: %w", err)
	}
	s.base = start
	return nil
}

// Written reports the total number of bytes ever appended.
func (s *Store) Written() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// Counts reports the total bytes the task ever produced and the bytes still
// held on disk. They are read together under one lock, so a caller can
// subtract them and never see two different moments.
func (s *Store) Counts() (written, retained int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written, s.written - s.base
}

// ReadSince returns up to limit bytes starting at cursor. next is the cursor
// for the following call. truncated reports bytes that rotation discarded
// before the first byte returned. limit must be positive.
func (s *Store) ReadSince(cursor int64, limit int) (data []byte, next int64, truncated int64, err error) {
	if limit <= 0 {
		return nil, cursor, 0, fmt.Errorf("output: limit must be positive, got %d", limit)
	}
	if cursor < 0 {
		return nil, cursor, 0, fmt.Errorf("output: cursor must not be negative, got %d", cursor)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if cursor < s.base {
		truncated = s.base - cursor
		cursor = s.base
	}
	if cursor >= s.written {
		return nil, s.written, truncated, nil
	}

	n := s.written - cursor
	if int64(limit) < n {
		n = int64(limit)
	}
	buf := make([]byte, n)
	if _, err := s.file.ReadAt(buf, cursor-s.base); err != nil {
		return nil, cursor, truncated, fmt.Errorf("output: read at %d: %w", cursor, err)
	}
	return buf, cursor + n, truncated, nil
}

// Tail returns the last n lines still held on disk. lines must be positive.
// An empty log is not an error: Tail returns nil, nil.
func (s *Store) Tail(lines int) ([]byte, error) {
	if lines <= 0 {
		return nil, fmt.Errorf("output: lines must be positive, got %d", lines)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.written == s.base {
		return nil, nil
	}

	buf := make([]byte, s.written-s.base)
	if _, err := s.file.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("output: read for tail: %w", err)
	}

	trimmed := bytes.TrimSuffix(buf, []byte("\n"))
	split := bytes.Split(trimmed, []byte("\n"))
	if lines < len(split) {
		split = split[len(split)-lines:]
	}
	out := bytes.Join(split, []byte("\n"))
	return append(out, '\n'), nil
}

// Close releases the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("output: close %s: %w", s.path, err)
	}
	return nil
}

// fileSize reports the size of the log on disk. Tests use it to check the cap.
func (s *Store) fileSize() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("output: stat: %w", err)
	}
	return info.Size(), nil
}
