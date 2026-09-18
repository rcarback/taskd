// SPDX-License-Identifier: AGPL-3.0-or-later

// Package watch decides when to wake an agent that is waiting on a task.
//
// A Condition names one reason to wake. A Tap observes a task's output and
// reports the two conditions that depend on it. Wait composes a set of
// conditions over a set of tasks and returns the first one to fire.
package watch

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind names one wake condition type.
type Kind string

// The wake condition types. Every one of them leaves the task running:
// waking an agent and ending a task are separate decisions, and only the
// caller makes the second one.
const (
	KindExit    Kind = "exit"
	KindIdle    Kind = "idle"
	KindElapsed Kind = "elapsed"
	KindMatch   Kind = "match"
	KindLines   Kind = "lines"
)

// defaultIdleSeconds is the silence window applied when a caller names no
// conditions. Silence is the safety net: it catches a task that has hung,
// which elapsed time cannot distinguish from one that is merely slow.
const defaultIdleSeconds = 300

// Condition is one reason to wake a waiter.
//
// The fields a Condition uses depend on its Type, so most are empty for any
// given value. Validate reports the combinations that do not make sense,
// rather than letting a zero seconds fire a timer on its next tick.
type Condition struct {
	Type Kind `json:"type"`

	// Seconds applies to KindIdle and KindElapsed.
	Seconds int `json:"seconds,omitempty"`

	// Pattern and Name apply to KindMatch. Name is what the fired event
	// reports, so an agent that asked for several patterns can tell them
	// apart without re-matching the line itself.
	Pattern string `json:"pattern,omitempty"`
	Name    string `json:"name,omitempty"`

	// N applies to KindLines.
	N int `json:"n,omitempty"`
}

// Defaults returns the conditions that apply when a caller omits until.
//
// The result is a fresh slice on every call, so a caller that appends to it
// cannot corrupt the next caller's defaults.
func Defaults() []Condition {
	return []Condition{
		{Type: KindExit},
		{Type: KindIdle, Seconds: defaultIdleSeconds},
	}
}

// Validate reports the first condition that cannot fire as written.
func Validate(conds []Condition) error {
	for _, c := range conds {
		if err := c.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c Condition) validate() error {
	switch c.Type {
	case KindExit:
		return nil
	case KindIdle, KindElapsed:
		if c.Seconds <= 0 {
			return fmt.Errorf("watch: %s needs a positive seconds, got %d", c.Type, c.Seconds)
		}
		return nil
	case KindMatch:
		if c.Pattern == "" {
			return fmt.Errorf("watch: %s needs a pattern", c.Type)
		}
		if _, err := regexp.Compile(c.Pattern); err != nil {
			return fmt.Errorf("watch: cannot compile %s pattern %q: %w", c.Type, c.Pattern, err)
		}
		return nil
	case KindLines:
		if c.N <= 0 {
			return fmt.Errorf("watch: %s needs a positive n, got %d", c.Type, c.N)
		}
		return nil
	default:
		return fmt.Errorf("watch: unknown condition type %q", c.Type)
	}
}

// ParseUntil reads the command line spelling of a condition set.
//
// The form is a comma separated list, each entry either a bare type or a
// type with one argument after a colon: "exit,idle:300,lines:50". An empty
// string gives Defaults, so a caller can pass a flag straight through
// without testing it first.
func ParseUntil(s string) ([]Condition, error) {
	if strings.TrimSpace(s) == "" {
		return Defaults(), nil
	}

	var conds []Condition
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, arg, hasArg := strings.Cut(part, ":")

		c := Condition{Type: Kind(name)}
		switch c.Type {
		case KindExit:
			if hasArg {
				return nil, fmt.Errorf("watch: %s takes no argument, got %q", c.Type, arg)
			}
		case KindIdle, KindElapsed:
			n, err := strconv.Atoi(arg)
			if err != nil {
				return nil, fmt.Errorf("watch: %s needs a seconds argument, got %q", c.Type, arg)
			}
			c.Seconds = n
		case KindLines:
			n, err := strconv.Atoi(arg)
			if err != nil {
				return nil, fmt.Errorf("watch: %s needs an n argument, got %q", c.Type, arg)
			}
			c.N = n
		case KindMatch:
			// A regular expression can hold a comma, which this spelling
			// splits on. The socket API carries match conditions instead.
			return nil, fmt.Errorf("watch: %s is not available on the command line: a pattern may contain a comma", c.Type)
		default:
			return nil, fmt.Errorf("watch: unknown condition type %q", name)
		}
		conds = append(conds, c)
	}

	if err := Validate(conds); err != nil {
		return nil, err
	}
	return conds, nil
}
