// internal/clock/fake.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package clock

import (
	"sort"
	"sync"
	"time"
)

type waiter struct {
	at time.Time
	ch chan time.Time
}

// Fake is a Clock that only moves when Advance is called.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
}

// NewFake returns a Fake positioned at start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now reports the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that receives once the fake clock reaches d from now.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &waiter{at: f.now.Add(d), ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	return w.ch
}

// Advance moves the clock forward and fires every timer that comes due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now

	sort.Slice(f.waiters, func(i, j int) bool { return f.waiters[i].at.Before(f.waiters[j].at) })

	var due, pending []*waiter
	for _, w := range f.waiters {
		if !w.at.After(now) {
			due = append(due, w)
			continue
		}
		pending = append(pending, w)
	}
	f.waiters = pending
	f.mu.Unlock()

	for _, w := range due {
		w.ch <- w.at
		close(w.ch)
	}
}
