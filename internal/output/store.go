// SPDX-License-Identifier: AGPL-3.0-or-later

// Package output stores task output in a bounded, append-only log and serves
// reads by absolute stream offset.
package output

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// Store holds the output of one task.
//
// Callers address the log by absolute stream offset. Offsets keep counting
// across rotation, so a cursor stays meaningful after old bytes are dropped.
type Store struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	maxBytes int64
	written  int64 // total bytes ever appended
	base     int64 // stream offset of the first byte still on disk
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

// Append writes p to the log, rotating first if the log would exceed its cap.
func (s *Store) Append(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.file.Write(p); err != nil {
		return fmt.Errorf("output: write to %s: %w", s.path, err)
	}
	s.written += int64(len(p))

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

// ReadSince returns up to limit bytes starting at cursor. next is the cursor
// for the following call. truncated reports bytes that rotation discarded
// before the first byte returned.
func (s *Store) ReadSince(cursor int64, limit int) (data []byte, next int64, truncated int64, err error) {
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

// Tail returns the last n lines still held on disk.
func (s *Store) Tail(lines int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
