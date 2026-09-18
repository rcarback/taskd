// SPDX-License-Identifier: AGPL-3.0-or-later

// Package clock provides a time source that tests can control.
package clock

import "time"

// Clock reports the current time and delivers timers.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// System returns the real clock.
func System() Clock { return systemClock{} }
