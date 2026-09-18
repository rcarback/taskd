// SPDX-License-Identifier: AGPL-3.0-or-later

package clock

import (
	"testing"
	"time"
)

func TestFakeAdvanceFiresTimer(t *testing.T) {
	start := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	c := NewFake(start)

	ch := c.After(30 * time.Second)

	select {
	case <-ch:
		t.Fatal("timer fired before the clock advanced")
	default:
	}

	c.Advance(30 * time.Second)

	select {
	case got := <-ch:
		if !got.Equal(start.Add(30 * time.Second)) {
			t.Fatalf("timer reported %v, want %v", got, start.Add(30*time.Second))
		}
	case <-time.After(time.Second):
		t.Fatal("timer did not fire after the clock advanced")
	}

	if got := c.Now(); !got.Equal(start.Add(30 * time.Second)) {
		t.Fatalf("Now() = %v, want %v", got, start.Add(30*time.Second))
	}
}

func TestFakeDoesNotFireEarly(t *testing.T) {
	c := NewFake(time.Unix(0, 0))
	ch := c.After(time.Minute)
	c.Advance(59 * time.Second)

	select {
	case <-ch:
		t.Fatal("timer fired one second early")
	default:
	}
}

func TestFakeTimerFiresOnlyOnce(t *testing.T) {
	c := NewFake(time.Unix(0, 0))
	ch := c.After(time.Minute)
	c.Advance(time.Minute)

	if got := <-ch; !got.Equal(time.Unix(0, 0).Add(time.Minute)) {
		t.Fatalf("timer reported %v, want %v", got, time.Unix(0, 0).Add(time.Minute))
	}

	select {
	case got := <-ch:
		t.Fatalf("timer delivered a second value %v, want no second delivery", got)
	default:
	}
}
