// SPDX-License-Identifier: AGPL-3.0-or-later

package output

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

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
