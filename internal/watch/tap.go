// SPDX-License-Identifier: AGPL-3.0-or-later

package watch

import (
	"bytes"
	"io"
	"regexp"
	"sync"
	"time"

	"github.com/rcarback/taskd/internal/ansi"
	"github.com/rcarback/taskd/internal/clock"
)

// Event reports one fired condition.
type Event struct {
	// TaskID is filled in by Wait, which knows which task the tap belongs
	// to. A Tap serves one task and does not carry its id.
	TaskID string `json:"id"`

	Kind Kind `json:"fired"`

	// Name is the caller's label for a KindMatch condition.
	Name string `json:"name,omitempty"`

	// Line is the matching line for KindMatch, already stripped of escape
	// sequences.
	Line string `json:"line,omitempty"`

	// Groups holds the capture groups of a KindMatch hit, excluding the
	// whole match. It is never nil: a nil slice marshals to JSON null, and
	// a client iterating it would have to test for that.
	Groups []string `json:"groups,omitempty"`

	// Count is the line total for KindLines.
	Count int64 `json:"count,omitempty"`

	// Seconds is the window that elapsed for KindIdle and KindElapsed.
	Seconds int `json:"seconds,omitempty"`
}

// Tap is the sink for one task's output. It writes every byte through to the
// underlying store unchanged, and observes a stripped copy so that pattern
// and line conditions can fire.
//
// Writes arrive from one goroutine, the supervisor's output copier, but
// waiters register and cancel from connection goroutines. Everything below
// mu is therefore guarded, including the sends to a waiter's channel: each
// channel is buffered for the single event it will ever carry and is closed
// in the same step, so no send under the lock can block. See matchLine.
type Tap struct {
	sink io.Writer
	clk  clock.Clock

	mu sync.Mutex
	// pending holds trailing bytes that begin an escape sequence this
	// chunk does not finish. ansi.Strip reports their length and excludes
	// them from clean, so they must be carried into the next Write or the
	// sequence would be matched as literal text.
	pending []byte
	// partial holds the bytes after the last newline: a line is not a line
	// until it ends, and a pattern may straddle a chunk boundary.
	partial   []byte
	lines     int64
	lastWrite time.Time

	matchers []*matcher
	counters []*counter
}

type matcher struct {
	name string
	re   *regexp.Regexp
	ch   chan Event
	done bool
}

type counter struct {
	target int64
	ch     chan Event
	done   bool
}

// NewTap returns a Tap that writes through to sink.
func NewTap(sink io.Writer, clk clock.Clock) *Tap {
	return &Tap{sink: sink, clk: clk, lastWrite: clk.Now()}
}

// Write sends p to the sink unchanged, then observes it.
//
// A sink error is returned without observing the bytes: they did not reach
// the log, so counting them would make the tap's line total disagree with
// what a later read can actually see.
func (t *Tap) Write(p []byte) (int, error) {
	if err := t.writeThrough(p); err != nil {
		return 0, err
	}
	t.observe(p)
	return len(p), nil
}

func (t *Tap) writeThrough(p []byte) error {
	n, err := t.sink.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

// observe strips p, splits it into complete lines, and fires any waiter the
// new lines satisfy.
func (t *Tap) observe(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.lastWrite = t.clk.Now()

	buf := make([]byte, 0, len(t.pending)+len(p))
	buf = append(append(buf, t.pending...), p...)
	clean, pendingLen := ansi.Strip(buf)
	t.pending = append([]byte(nil), buf[len(buf)-pendingLen:]...)

	t.partial = append(t.partial, clean...)

	for {
		i := bytes.IndexByte(t.partial, '\n')
		if i < 0 {
			break
		}
		line := string(t.partial[:i])
		t.partial = t.partial[i+1:]
		t.lines++
		t.matchLine(line)
		t.countLine()
	}
}

// matchLine tests every live matcher against line. The caller holds mu.
//
// Sending under the lock is safe and deliberate: every waiter channel is
// buffered for exactly the one event it will ever carry, and the waiter is
// marked done and closed in the same step, so no send here can block. A
// later change that unbuffers these channels would deadlock, which is why
// the buffering is stated rather than assumed.
func (t *Tap) matchLine(line string) {
	for _, m := range t.matchers {
		if m.done {
			continue
		}
		groups := m.re.FindStringSubmatch(line)
		if groups == nil {
			continue
		}
		m.done = true
		m.ch <- Event{
			Kind:   KindMatch,
			Name:   m.name,
			Line:   line,
			Groups: append([]string{}, groups[1:]...),
		}
		close(m.ch)
	}
}

// countLine fires every counter the new line total reaches. The caller holds
// mu. See matchLine on why sending under the lock cannot block.
func (t *Tap) countLine() {
	for _, c := range t.counters {
		if c.done || t.lines < c.target {
			continue
		}
		c.done = true
		c.ch <- Event{Kind: KindLines, Count: t.lines}
		close(c.ch)
	}
}

// OnMatch registers a waiter for the first line matching re.
//
// The returned channel carries at most one event and is then closed, so a
// caller may select on it without tracking whether it already fired. The
// cancel function unregisters the waiter and closes the channel; calling it
// after the waiter fired is safe and does nothing.
func (t *Tap) OnMatch(name string, re *regexp.Regexp) (<-chan Event, func()) {
	m := &matcher{name: name, re: re, ch: make(chan Event, 1)}

	t.mu.Lock()
	t.matchers = append(t.matchers, m)
	t.mu.Unlock()

	return m.ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if m.done {
			return
		}
		m.done = true
		close(m.ch)
		t.dropMatcher(m)
	}
}

// OnLines registers a waiter for the moment the task's line total reaches n.
func (t *Tap) OnLines(n int64) (<-chan Event, func()) {
	c := &counter{target: n, ch: make(chan Event, 1)}

	t.mu.Lock()
	already := t.lines >= n
	if already {
		// The threshold is already behind us. Fire at once rather than
		// wait for a line that may never come.
		c.done = true
		c.ch <- Event{Kind: KindLines, Count: t.lines}
		close(c.ch)
	} else {
		t.counters = append(t.counters, c)
	}
	t.mu.Unlock()

	return c.ch, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if c.done {
			return
		}
		c.done = true
		close(c.ch)
		t.dropCounter(c)
	}
}

// dropMatcher and dropCounter remove a finished waiter so a long-lived task
// does not accumulate one entry per wait call it ever served. The caller
// holds mu.
func (t *Tap) dropMatcher(m *matcher) {
	for i, cur := range t.matchers {
		if cur == m {
			t.matchers = append(t.matchers[:i], t.matchers[i+1:]...)
			return
		}
	}
}

func (t *Tap) dropCounter(c *counter) {
	for i, cur := range t.counters {
		if cur == c {
			t.counters = append(t.counters[:i], t.counters[i+1:]...)
			return
		}
	}
}

// Lines reports how many complete lines the task has written.
func (t *Tap) Lines() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lines
}

// LastWrite reports when output last arrived. Before the first write it
// reports the tap's construction time, so an idle window measured against it
// starts when the task started rather than at the zero time.
func (t *Tap) LastWrite() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastWrite
}
