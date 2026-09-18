// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"bytes"
	"regexp"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/watch"
)

func TestTapPassesRawBytesThrough(t *testing.T) {
	// The log must hold exactly what the task produced, escape sequences
	// and all. Only the matching view is stripped.
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	raw := []byte("\x1b[31merror:\x1b[0m boom\n")
	n, err := tap.Write(raw)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(raw) {
		t.Errorf("Write returned %d, want %d: a short count makes io.Copy report a failure", n, len(raw))
	}
	if got := sink.Bytes(); !bytes.Equal(got, raw) {
		t.Errorf("sink holds %q, want the raw bytes %q", got, raw)
	}
}

func TestTapCountsCompleteLinesOnly(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	if _, err := tap.Write([]byte("one\ntwo\nthr")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.Lines(); got != 2 {
		t.Errorf("Lines() = %d, want 2: the third line has no newline yet", got)
	}
	if _, err := tap.Write([]byte("ee\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.Lines(); got != 3 {
		t.Errorf("Lines() = %d, want 3 once the line completes", got)
	}
}

func TestTapMatchesAcrossAChunkBoundary(t *testing.T) {
	// A pattern that straddles two writes must still match. Splitting on
	// arrival rather than on lines is the classic way to miss it.
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error:`))
	defer cancel()

	if _, err := tap.Write([]byte("bui")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("fired early on a partial line: %+v", e)
	default:
	}

	if _, err := tap.Write([]byte("ld error: boom\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		if e.Kind != watch.KindMatch {
			t.Errorf("Kind = %q, want %q", e.Kind, watch.KindMatch)
		}
		if e.Name != "err" {
			t.Errorf("Name = %q, want %q", e.Name, "err")
		}
		if e.Line != "build error: boom" {
			t.Errorf("Line = %q, want the whole joined line", e.Line)
		}
	default:
		t.Fatal("no event: the pattern straddled the chunk boundary and was missed")
	}
}

func TestTapStripsEscapesBeforeMatching(t *testing.T) {
	// A colour code sitting inside the word must not stop the match.
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`^error: boom$`))
	defer cancel()

	if _, err := tap.Write([]byte("\x1b[31merror:\x1b[0m boom\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		if e.Line != "error: boom" {
			t.Errorf("Line = %q, want the stripped line", e.Line)
		}
	default:
		t.Fatal("no event: the escape sequences were not stripped before matching")
	}
}

func TestTapMatchFiresOnce(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error`))
	defer cancel()

	if _, err := tap.Write([]byte("error one\nerror two\nerror three\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := <-events; !ok {
		t.Fatal("channel closed without an event")
	}
	select {
	case e, ok := <-events:
		if ok {
			t.Fatalf("second event %+v: a waiter fires once and unregisters", e)
		}
	default:
		t.Fatal("channel still open after firing: the tap must close it so a waiter cannot block")
	}
}

func TestTapOnLinesFiresAtTheThreshold(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnLines(3)
	defer cancel()

	if _, err := tap.Write([]byte("a\nb\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("fired at 2 lines: %+v", e)
	default:
	}

	if _, err := tap.Write([]byte("c\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		if e.Kind != watch.KindLines {
			t.Errorf("Kind = %q, want %q", e.Kind, watch.KindLines)
		}
		if e.Count != 3 {
			t.Errorf("Count = %d, want 3", e.Count)
		}
	default:
		t.Fatal("no event at the threshold")
	}
}

func TestTapLastWriteTracksTheClock(t *testing.T) {
	start := time.Unix(1_000, 0)
	fake := clock.NewFake(start)
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, fake)

	if got := tap.LastWrite(); !got.Equal(start) {
		t.Errorf("LastWrite() = %v before any write, want the start time %v", got, start)
	}

	fake.Advance(30 * time.Second)
	if _, err := tap.Write([]byte("x\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := tap.LastWrite(), start.Add(30*time.Second); !got.Equal(want) {
		t.Errorf("LastWrite() = %v, want %v", got, want)
	}
}

func TestTapCancelUnregisters(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error`))
	cancel()

	if _, err := tap.Write([]byte("error here\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := <-events; ok {
		t.Fatal("a cancelled waiter received an event")
	}
}
