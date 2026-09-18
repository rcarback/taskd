// SPDX-License-Identifier: AGPL-3.0-or-later

package watch

import (
	"regexp"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
)

// This file is a white-box test in package watch, not watch_test: it reads
// the unexported matchers/counters slices directly, which is the most
// direct way to check that a fired waiter removes itself rather than
// leaking for the life of the Tap. Everything else about the Tap is
// covered from outside the package in tap_test.go.

func TestTapDropsAFiredMatcher(t *testing.T) {
	tap := NewTap(discard{}, clock.NewFake(time.Unix(0, 0)), nil)

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error`))
	defer cancel()

	tap.mu.Lock()
	before := len(tap.matchers)
	tap.mu.Unlock()
	if before != 1 {
		t.Fatalf("matchers before firing = %d, want 1", before)
	}

	if _, err := tap.Write([]byte("error one\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := <-events; !ok {
		t.Fatal("channel closed without an event")
	}

	tap.mu.Lock()
	after := len(tap.matchers)
	tap.mu.Unlock()
	if after != 0 {
		t.Errorf("matchers after firing = %d, want 0: a fired matcher must drop itself", after)
	}
}

func TestTapDropsAFiredCounter(t *testing.T) {
	tap := NewTap(discard{}, clock.NewFake(time.Unix(0, 0)), nil)

	events, cancel := tap.OnLines(1)
	defer cancel()

	tap.mu.Lock()
	before := len(tap.counters)
	tap.mu.Unlock()
	if before != 1 {
		t.Fatalf("counters before firing = %d, want 1", before)
	}

	if _, err := tap.Write([]byte("one\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := <-events; !ok {
		t.Fatal("channel closed without an event")
	}

	tap.mu.Lock()
	after := len(tap.counters)
	tap.mu.Unlock()
	if after != 0 {
		t.Errorf("counters after firing = %d, want 0: a fired counter must drop itself", after)
	}
}

// TestTapMatchersStayBoundedAcrossManyWaiters exercises the same fix from
// the outside: a long-lived task that serves one OnMatch call per line does
// not accumulate a matchers entry per call it already served, only for the
// ones still pending.
func TestTapMatchersStayBoundedAcrossManyWaiters(t *testing.T) {
	tap := NewTap(discard{}, clock.NewFake(time.Unix(0, 0)), nil)

	// Register and immediately satisfy ten matchers, one per line.
	for i := range 10 {
		events, cancel := tap.OnMatch("err", regexp.MustCompile(`error`))
		defer cancel()
		if _, err := tap.Write([]byte("error\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if _, ok := <-events; !ok {
			t.Fatalf("waiter %d: channel closed without an event", i)
		}
	}

	// One more, left pending, so the slice is not simply empty by
	// coincidence.
	_, cancel := tap.OnMatch("pending", regexp.MustCompile(`never`))
	defer cancel()

	tap.mu.Lock()
	got := len(tap.matchers)
	tap.mu.Unlock()
	if got != 1 {
		t.Errorf("matchers = %d, want 1: only the still-pending waiter should remain", got)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
