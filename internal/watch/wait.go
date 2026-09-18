// SPDX-License-Identifier: AGPL-3.0-or-later

package watch

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/rcarback/taskd/internal/clock"
)

// Source is one task a caller is waiting on.
//
// The daemon implements this over its own Entry. Keeping it an interface is
// what lets this package be tested without a daemon, a socket, or a real
// process.
type Source interface {
	ID() string
	Tap() *Tap
	Done() <-chan struct{}
}

// Wait blocks until one condition fires on one source, and reports which.
//
// Every condition is armed against every source, so a caller watching three
// builds for "exit or 300 seconds of silence" writes one call. The first to
// fire wins and the rest are released.
//
// Wait never ends a task. A fired condition is a report, not an action.
func Wait(ctx context.Context, clk clock.Clock, srcs []Source, until []Condition) (Event, error) {
	if len(srcs) == 0 {
		return Event{}, fmt.Errorf("watch: wait needs at least one task")
	}
	if len(until) == 0 {
		until = Defaults()
	}
	if err := Validate(until); err != nil {
		return Event{}, err
	}

	// Compiled once here, before any goroutine starts, rather than once
	// per (source, condition) pair inside arm. Validate above already
	// proved every pattern compiles, so this cannot fail in practice, but
	// a failure inside a spawned goroutine would panic with no recover and
	// take the whole daemon down instead of returning an error.
	compiled := make([]*regexp.Regexp, len(until))
	for i, cond := range until {
		if cond.Type == KindMatch {
			re, err := regexp.Compile(cond.Pattern)
			if err != nil {
				return Event{}, fmt.Errorf("watch: %w", err)
			}
			compiled[i] = re
		}
	}

	// Buffered for every armed condition, so a loser that fires while the
	// winner is being returned does not block forever on an unread send
	// and leak its goroutine.
	fired := make(chan Event, len(srcs)*len(until))

	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	for _, src := range srcs {
		for i, cond := range until {
			wg.Add(1)
			go func(src Source, cond Condition, re *regexp.Regexp) {
				defer wg.Done()
				arm(ctx, clk, src, cond, re, fired)
			}(src, cond, compiled[i])
		}
	}

	// retired closes once every armed goroutine has returned without a
	// win. KindMatch and KindLines retire when their source exits before
	// matching, and KindIdle retires when its source exits mid-window (see
	// arm and armIdle below). Without this, a caller who asked for one of
	// those alone on a task that then ends gets no reply at all: fired
	// stays empty forever and the only other live case is ctx.Done, which
	// nothing here would ever close.
	//
	// A second goroutine calling wg.Wait is legal — sync.WaitGroup allows
	// any number of concurrent waiters — so this does not conflict with
	// the teardown Wait below. It is safe against wg.Add too: every Add
	// above happened before this goroutine started, and nothing calls Add
	// after this point.
	retired := make(chan struct{})
	go func() {
		wg.Wait()
		close(retired)
	}()

	var result Event
	var waitErr error
	select {
	case result = <-fired:
	case <-ctx.Done():
		waitErr = ctx.Err()
	case <-retired:
		waitErr = fmt.Errorf("watch: no condition can still fire: every task ended before any condition was met")
	}

	// fired is buffered for every armed condition, so a real event can
	// still be waiting here even though the select above returned a ctx or
	// retired error instead: when two cases are ready at once, select
	// breaks the tie at random, not in the order they're written. A
	// non-blocking recheck prefers the genuine wake over either error.
	if waitErr != nil {
		select {
		case result = <-fired:
			waitErr = nil
		default:
		}
	}

	// cancel must run, and every armed goroutine must be given the chance
	// to observe it, before Wait returns: that is what releases a losing
	// exit/elapsed/idle goroutine (blocked on ctx.Done()) and drops a
	// losing match/lines tap registration (released in arm's own defer).
	// Calling cancel and wg.Wait here, rather than as two separate defers,
	// matters because defers run last-registered-first: a plain `defer
	// cancel()` followed by `defer wg.Wait()` would wait for the
	// goroutines *before* cancelling them, and a goroutine still waiting
	// on a losing source would then hang forever. Doing it inline keeps
	// the order explicit.
	//
	// The retired detector goroutine above is blocked on this same wg, so
	// it unblocks at the same moment this wg.Wait call does and closes
	// retired (once — it is the only goroutine that ever does so) shortly
	// after. Nothing below waits on retired, so its exit cannot hold Wait
	// up or leak: it runs to completion within one scheduling step of the
	// wg reaching zero either way.
	cancel()
	wg.Wait()

	return result, waitErr
}

// arm blocks on one condition for one source and reports it if it fires. re
// is the compiled pattern for a KindMatch condition, or nil for any other
// kind.
func arm(ctx context.Context, clk clock.Clock, src Source, cond Condition, re *regexp.Regexp, fired chan<- Event) {
	switch cond.Type {
	case KindExit:
		select {
		case <-src.Done():
			send(fired, Event{TaskID: src.ID(), Kind: KindExit})
		case <-ctx.Done():
		}

	case KindElapsed:
		select {
		case <-clk.After(time.Duration(cond.Seconds) * time.Second):
			send(fired, Event{TaskID: src.ID(), Kind: KindElapsed, Seconds: cond.Seconds})
		case <-ctx.Done():
		}

	case KindIdle:
		armIdle(ctx, clk, src, cond, fired)

	case KindMatch:
		name := cond.Name
		if name == "" {
			name = cond.Pattern
		}
		ch, release := src.Tap().OnMatch(name, re)
		defer release()
		select {
		case e, ok := <-ch:
			if ok {
				e.TaskID = src.ID()
				send(fired, e)
			}
		case <-src.Done():
			// The task ended without matching: retire rather than block
			// forever on a channel a Tap has no notion to ever close.
			drainOnRetire(ch, src.ID(), fired)
		case <-ctx.Done():
		}

	case KindLines:
		ch, release := src.Tap().OnLines(int64(cond.N))
		defer release()
		select {
		case e, ok := <-ch:
			if ok {
				e.TaskID = src.ID()
				send(fired, e)
			}
		case <-src.Done():
			// Same reasoning as KindMatch above: the line count will
			// never reach its target once the task has stopped writing.
			drainOnRetire(ch, src.ID(), fired)
		case <-ctx.Done():
		}

	default:
		// Validate rejects any Kind but the five handled above, so this
		// is unreachable through the public API. Panicking rather than
		// falling through silently matters here specifically: a kind that
		// arms nothing contributes neither a win nor a retirement, and
		// Wait would have no way to tell the difference from a condition
		// that is still legitimately pending.
		panic(fmt.Sprintf("watch: arm: condition type %q slipped past Validate", cond.Type))
	}
}

// drainOnRetire recovers an event that won the race against src.Done() only
// by losing it: the outer select in arm breaks a tie between two
// simultaneously ready channels at random, and the daemon drains a task's
// output through its tap before closing Done, so a pattern or line count
// that was satisfied by the task's last write can already be sitting in ch
// at the exact moment Done closes. A non-blocking receive here recovers it
// instead of discarding it as a plain retirement.
func drainOnRetire(ch <-chan Event, srcID string, fired chan<- Event) {
	select {
	case e, ok := <-ch:
		if ok {
			e.TaskID = srcID
			send(fired, e)
		}
	default:
	}
}

// armIdle fires after cond.Seconds of silence, measured from the task's last
// write rather than from the start of the wait.
//
// It re-arms rather than setting one timer for the whole window: a task that
// prints at second 299 of a 300 second window has not been silent, and a
// single timer would wake the agent anyway.
func armIdle(ctx context.Context, clk clock.Clock, src Source, cond Condition, fired chan<- Event) {
	window := time.Duration(cond.Seconds) * time.Second
	for {
		quiet := clk.Now().Sub(src.Tap().LastWrite())
		if quiet >= window {
			send(fired, Event{TaskID: src.ID(), Kind: KindIdle, Seconds: cond.Seconds})
			return
		}
		select {
		case <-clk.After(window - quiet):
		case <-src.Done():
			// The task ended before the window elapsed. A task that
			// exited is not hung — that is exactly what idle exists to
			// detect — so this retires without firing rather than
			// reporting a hang that never happened. Wait's retired
			// channel is what turns this into a reply instead of a
			// silent hang when idle is the only condition armed.
			return
		case <-ctx.Done():
			return
		}
	}
}

// send reports an event without blocking. fired is buffered for every armed
// condition, so this never drops one that a caller could still read.
func send(fired chan<- Event, e Event) {
	select {
	case fired <- e:
	default:
	}
}
