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

	// Buffered for every armed condition, so a loser that fires while the
	// winner is being returned does not block forever on an unread send
	// and leak its goroutine.
	fired := make(chan Event, len(srcs)*len(until))

	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	for _, src := range srcs {
		for _, cond := range until {
			wg.Add(1)
			go func(src Source, cond Condition) {
				defer wg.Done()
				arm(ctx, clk, src, cond, fired)
			}(src, cond)
		}
	}

	var result Event
	var waitErr error
	select {
	case result = <-fired:
	case <-ctx.Done():
		waitErr = ctx.Err()
	}

	// cancel must run, and every armed goroutine must be given the chance
	// to observe it, before Wait returns: that is what releases a losing
	// exit/elapsed goroutine (blocked on ctx.Done()) and drops a losing
	// match/lines tap registration (released in arm's own defer). Calling
	// cancel and wg.Wait here, rather than as two separate defers, matters
	// because defers run last-registered-first: a plain `defer cancel()`
	// followed by `defer wg.Wait()` would wait for the goroutines *before*
	// cancelling them, and a goroutine still waiting on a losing source
	// would then hang forever. Doing it inline keeps the order explicit.
	cancel()
	wg.Wait()

	return result, waitErr
}

// arm blocks on one condition for one source and reports it if it fires.
func arm(ctx context.Context, clk clock.Clock, src Source, cond Condition, fired chan<- Event) {
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
		// Validate already compiled this successfully.
		re := regexp.MustCompile(cond.Pattern)
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
		case <-ctx.Done():
		}
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
			// The task ended. Silence after that is not a hang, and an
			// exit condition (armed separately when the caller asked for
			// one) is the honest report.
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
