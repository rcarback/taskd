// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/watch"
)

// fakeSource is a task under test. The daemon supplies the real one.
type fakeSource struct {
	id   string
	tap  *watch.Tap
	done chan struct{}
}

func newFakeSource(id string, clk clock.Clock) *fakeSource {
	return &fakeSource{id: id, tap: watch.NewTap(&bytes.Buffer{}, clk, nil), done: make(chan struct{})}
}

func (f *fakeSource) ID() string            { return f.id }
func (f *fakeSource) Tap() *watch.Tap       { return f.tap }
func (f *fakeSource) Done() <-chan struct{} { return f.done }

func TestWaitFiresOnExit(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	go close(src.done)

	e, err := watch.Wait(t.Context(), fake, []watch.Source{src}, []watch.Condition{{Type: watch.KindExit}})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if e.Kind != watch.KindExit {
		t.Errorf("Kind = %q, want %q", e.Kind, watch.KindExit)
	}
	if e.TaskID != "t1" {
		t.Errorf("TaskID = %q, want %q: the caller must know which task fired", e.TaskID, "t1")
	}
}

func TestWaitFiresOnElapsedAndLeavesTheTaskRunning(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	type result struct {
		e   watch.Event
		err error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := watch.Wait(t.Context(), fake, []watch.Source{src},
			[]watch.Condition{{Type: watch.KindElapsed, Seconds: 600}})
		ch <- result{e, err}
	}()

	fake.BlockUntil(1)
	fake.Advance(600 * time.Second)

	got := <-ch
	if got.err != nil {
		t.Fatalf("Wait: %v", got.err)
	}
	if got.e.Kind != watch.KindElapsed {
		t.Errorf("Kind = %q, want %q", got.e.Kind, watch.KindElapsed)
	}
	if got.e.Seconds != 600 {
		t.Errorf("Seconds = %d, want 600", got.e.Seconds)
	}
	select {
	case <-src.done:
		t.Fatal("the task ended: elapsed wakes the waiter and leaves the task running")
	default:
	}
}

func TestWaitIdleReArmsAfterOutput(t *testing.T) {
	// The bug this guards: arming one timer for the whole window, so a
	// task that printed at second 299 still wakes the agent at 300.
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	type result struct {
		e   watch.Event
		err error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := watch.Wait(t.Context(), fake, []watch.Source{src},
			[]watch.Condition{{Type: watch.KindIdle, Seconds: 300}})
		ch <- result{e, err}
	}()

	fake.BlockUntil(1)
	fake.Advance(200 * time.Second)

	if _, err := src.tap.Write([]byte("still alive\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fake.BlockUntil(1)
	fake.Advance(200 * time.Second) // 400s total, but only 200s of silence

	// BlockUntil syncs to the goroutine registering its next timer, which
	// only happens after it has evaluated this tick and decided not to
	// fire. Checking ch immediately after Advance, without this sync,
	// would race the goroutine under test: a single-timer implementation's
	// send might not have reached ch yet, letting this assertion pass
	// against exactly the bug it exists to catch.
	//
	// Against that same single-timer bug, the re-arm this BlockUntil is
	// waiting for never happens, so it must not block the test directly:
	// that would hang until the whole test binary's timeout and dump a
	// stack instead of running the t.Fatalf below. Racing it against ch
	// in a select lets a buggy implementation's eventual send win instead.
	synced := make(chan struct{})
	go func() {
		fake.BlockUntil(1)
		close(synced)
	}()
	select {
	case got := <-ch:
		t.Fatalf("fired after 200s of silence: %+v", got.e)
	case <-synced:
	}

	fake.BlockUntil(1)
	fake.Advance(100 * time.Second) // now 300s since the write
	got := <-ch
	if got.err != nil {
		t.Fatalf("Wait: %v", got.err)
	}
	if got.e.Kind != watch.KindIdle {
		t.Errorf("Kind = %q, want %q", got.e.Kind, watch.KindIdle)
	}
}

func TestWaitFiresOnMatch(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	type result struct {
		e   watch.Event
		err error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := watch.Wait(t.Context(), fake, []watch.Source{src},
			[]watch.Condition{{Type: watch.KindMatch, Pattern: `error:`, Name: "err"}})
		ch <- result{e, err}
	}()

	// Wait registers its matcher before it blocks, but a test cannot see
	// that moment. Write in a loop until the event arrives.
	deadline := time.After(5 * time.Second)
	for {
		if _, err := src.tap.Write([]byte("error: boom\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		select {
		case got := <-ch:
			if got.err != nil {
				t.Fatalf("Wait: %v", got.err)
			}
			if got.e.Kind != watch.KindMatch {
				t.Errorf("Kind = %q, want %q", got.e.Kind, watch.KindMatch)
			}
			if got.e.Name != "err" {
				t.Errorf("Name = %q, want %q", got.e.Name, "err")
			}
			return
		case <-deadline:
			t.Fatal("no match event within 5s")
		default:
		}
	}
}

func TestWaitReportsTheFirstOfSeveralTasks(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	a := newFakeSource("a", fake)
	b := newFakeSource("b", fake)

	close(b.done)

	e, err := watch.Wait(t.Context(), fake, []watch.Source{a, b}, []watch.Condition{{Type: watch.KindExit}})
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if e.TaskID != "b" {
		t.Errorf("TaskID = %q, want %q: the call must name the task that fired", e.TaskID, "b")
	}
}

func TestWaitReturnsContextError(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := watch.Wait(ctx, fake, []watch.Source{src}, []watch.Condition{{Type: watch.KindExit}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled: a client that hung up must release the handler", err)
	}
}

func TestWaitRejectsNoSources(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	_, err := watch.Wait(t.Context(), fake, nil, watch.Defaults())
	if err == nil {
		t.Fatal("Wait with no sources returned nil, want an error")
	}
}

// TestWaitReturnsErrorWhenIdleSourceExits guards the hang Finding 1
// describes: idle correctly never fires once its source has exited (a task
// that ended is not a hang), but with only idle armed that used to leave
// Wait with no live case but ctx.Done and no way to ever return.
func TestWaitReturnsErrorWhenIdleSourceExits(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	go close(src.done)

	type result struct {
		e   watch.Event
		err error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := watch.Wait(t.Context(), fake, []watch.Source{src},
			[]watch.Condition{{Type: watch.KindIdle, Seconds: 300}})
		ch <- result{e, err}
	}()

	select {
	case got := <-ch:
		if !errors.Is(got.err, watch.ErrNoConditionCanFire) {
			t.Fatalf("Wait error = %v, want ErrNoConditionCanFire: idle must not fire once its source has exited", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s: every armed condition retired without firing, so it must not hang")
	}
}

// TestWaitReturnsErrorWhenMatchSourceExitsWithoutMatch covers the same
// class of hang for KindMatch: a Tap has no notion of task end, so nothing
// ever closes the waiter channel OnMatch returns, and match relied entirely
// on Wait's retirement to notice its source is gone.
func TestWaitReturnsErrorWhenMatchSourceExitsWithoutMatch(t *testing.T) {
	fake := clock.NewFake(time.Unix(0, 0))
	src := newFakeSource("t1", fake)

	go close(src.done)

	type result struct {
		e   watch.Event
		err error
	}
	ch := make(chan result, 1)
	go func() {
		e, err := watch.Wait(t.Context(), fake, []watch.Source{src},
			[]watch.Condition{{Type: watch.KindMatch, Pattern: `error:`, Name: "err"}})
		ch <- result{e, err}
	}()

	select {
	case got := <-ch:
		if !errors.Is(got.err, watch.ErrNoConditionCanFire) {
			t.Fatalf("Wait error = %v, want ErrNoConditionCanFire: match must not hang once its source has exited without matching", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s: every armed condition retired without firing, so it must not hang")
	}
}
