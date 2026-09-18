// SPDX-License-Identifier: AGPL-3.0-or-later

package output

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// partialWriteFile wraps a real file but caps each Write at n bytes and then
// reports err, simulating a disk that stops accepting data mid-write
// (ENOSPC, EFBIG, EIO) — conditions a real file in a test cannot reliably
// reproduce on demand. The capped bytes still land on the underlying file,
// matching what a genuine partial write does.
type partialWriteFile struct {
	*os.File
	n   int
	err error
}

func (f *partialWriteFile) Write(p []byte) (int, error) {
	if len(p) <= f.n {
		return f.File.Write(p)
	}
	written, err := f.File.Write(p[:f.n])
	if err != nil {
		return written, err
	}
	return written, f.err
}

func open(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "out.log"), maxBytes)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestReadSinceReturnsEachByteExactlyOnce(t *testing.T) {
	s := open(t, 1<<20)
	chunks := []string{"alpha\n", "beta\n", "gamma\n"}
	for _, c := range chunks {
		if err := s.Append([]byte(c)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var got bytes.Buffer
	cursor := int64(0)
	for {
		data, next, truncated, err := s.ReadSince(cursor, 4)
		if err != nil {
			t.Fatalf("ReadSince: %v", err)
		}
		if truncated != 0 {
			t.Fatalf("unexpected truncation of %d bytes", truncated)
		}
		if len(data) == 0 {
			break
		}
		got.Write(data)
		cursor = next
	}

	want := strings.Join(chunks, "")
	if got.String() != want {
		t.Fatalf("stream = %q, want %q", got.String(), want)
	}
	if cursor != int64(len(want)) {
		t.Fatalf("final cursor = %d, want %d", cursor, len(want))
	}
}

func TestWrittenCountsEveryByteAcrossRotation(t *testing.T) {
	s := open(t, 64)
	line := []byte("0123456789abcdef\n") // 17 bytes
	for range 20 {
		if err := s.Append(line); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	want := int64(len(line) * 20)
	if got := s.Written(); got != want {
		t.Fatalf("Written() = %d, want %d", got, want)
	}
}

func TestAppendAdvancesWrittenByAPartialWriteEvenOnError(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "out.log")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { _ = tmp.Close() })

	s := &Store{
		path:     tmp.Name(),
		file:     &partialWriteFile{File: tmp, n: 3, err: fmt.Errorf("disk full")},
		maxBytes: 1 << 20,
	}

	if err := s.Append([]byte("0123456789")); err == nil {
		t.Fatal("Append: want an error from a partial write")
	}

	// The file only accepted the first 3 bytes before failing; written must
	// track that, not the 10 bytes the caller asked to append, or a later
	// ReadSince would read the stream at the wrong offset.
	if got := s.Written(); got != 3 {
		t.Fatalf("Written() = %d, want 3 (the bytes actually written before the failure)", got)
	}
}

func TestRotationBoundsTheFileAndReportsTruncation(t *testing.T) {
	s := open(t, 64)
	for range 20 {
		if err := s.Append([]byte("0123456789abcdef\n")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	size, err := s.fileSize()
	if err != nil {
		t.Fatalf("fileSize: %v", err)
	}
	if size > 64 {
		t.Fatalf("file grew to %d bytes, cap is 64", size)
	}

	_, _, truncated, err := s.ReadSince(0, 1024)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if truncated == 0 {
		t.Fatal("ReadSince reported no truncation after rotation")
	}
}

func TestTailReturnsLastLines(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Tail(2)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if string(got) != "three\nfour\n" {
		t.Fatalf("Tail(2) = %q, want %q", got, "three\nfour\n")
	}
}

func TestReadSinceAtEndReturnsNothing(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("data\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, next, _, err := s.ReadSince(s.Written(), 1024)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected no data, got %q", data)
	}
	if next != s.Written() {
		t.Fatalf("next = %d, want %d", next, s.Written())
	}
}

func TestReadSinceRejectsNonPositiveLimit(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("data\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, limit := range []int{0, -1} {
		data, next, truncated, err := s.ReadSince(0, limit)
		if err == nil {
			t.Fatalf("ReadSince(0, %d): expected error, got data=%q next=%d truncated=%d",
				limit, data, next, truncated)
		}
		if data != nil {
			t.Fatalf("ReadSince(0, %d): expected nil data on error, got %q", limit, data)
		}
		if next != 0 {
			t.Fatalf("ReadSince(0, %d): next = %d, want unchanged cursor 0", limit, next)
		}
	}
}

func TestReadSinceRejectsANegativeCursor(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("data\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, cursor := range []int64{-1, math.MinInt64} {
		data, next, truncated, err := s.ReadSince(cursor, 1024)
		if err == nil {
			t.Fatalf("ReadSince(%d, 1024): expected error, got data=%q next=%d truncated=%d",
				cursor, data, next, truncated)
		}
		if data != nil {
			t.Fatalf("ReadSince(%d, 1024): expected nil data on error, got %q", cursor, data)
		}
		if next != cursor {
			t.Fatalf("ReadSince(%d, 1024): next = %d, want unchanged cursor %d", cursor, next, cursor)
		}
	}
}

func TestTailOnEmptyLogReturnsNoBytes(t *testing.T) {
	s := open(t, 1<<20)

	got, err := s.Tail(5)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Tail(5) on empty log = %q, want no bytes", got)
	}
}

func TestTailRejectsNonPositiveLines(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("one\ntwo\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, lines := range []int{0, -1} {
		if _, err := s.Tail(lines); err == nil {
			t.Fatalf("Tail(%d): expected error, got nil", lines)
		}
	}
}

// TestReadSinceAcrossRotationMatchesModel guards the reason this package
// exists: a store that tracked file offsets instead of stream offsets could
// still pass the tests above (they never check returned byte content past a
// rotation). This test keeps a plain in-memory model of everything ever
// appended and checks the store's reads against it after many rotations.
func TestReadSinceAcrossRotationMatchesModel(t *testing.T) {
	const maxBytes = 80 // keep = 40 after each rotation
	s := open(t, maxBytes)

	var model []byte
	for i := range 40 {
		chunk := []byte(fmt.Sprintf("chunk-%04d\n", i)) // 11 bytes, each unique
		if err := s.Append(chunk); err != nil {
			t.Fatalf("Append: %v", err)
		}
		model = append(model, chunk...)
	}

	// 40 appends of 11 bytes = 440 bytes against an 80-byte cap: a rotation
	// fires every time written-base exceeds 80 and always drops back to 40,
	// so it must fire well over three times before the loop ends.

	var got bytes.Buffer
	cursor := int64(0)
	firstTruncated := int64(-1)
	lastWritten := int64(0)
	for {
		data, next, truncated, err := s.ReadSince(cursor, 16)
		if err != nil {
			t.Fatalf("ReadSince: %v", err)
		}
		if firstTruncated == -1 {
			firstTruncated = truncated
		}
		if w := s.Written(); w < lastWritten {
			t.Fatalf("Written() decreased: was %d, now %d", lastWritten, w)
		} else {
			lastWritten = w
		}
		if len(data) == 0 {
			if next != s.Written() {
				t.Fatalf("next = %d at end, want Written() = %d", next, s.Written())
			}
			break
		}
		got.Write(data)
		cursor = next
	}

	// 3*keep is a lower bound that only holds if at least three rotations
	// ran: each rotation can discard at most maxBytes-keep bytes beyond what
	// prior rotations already discarded, so surviving past 3*keep discarded
	// bytes requires more than three of them.
	if firstTruncated < 3*(maxBytes/2) {
		t.Fatalf("expected at least three rotations, only %d bytes discarded", firstTruncated)
	}

	want := model[firstTruncated:]
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("stream after rotation = %q, want %q", got.Bytes(), want)
	}
}
