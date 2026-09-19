// SPDX-License-Identifier: AGPL-3.0-or-later

package watch

import (
	"fmt"
	"regexp"
	"time"
)

// Action is what a start-time pattern does when it matches.
type Action string

// The pattern actions.
//
// ActionRecord is the cheap one and the reason patterns exist: it turns a
// log an agent would otherwise have to read into a counter and one line,
// which task_status returns in tens of bytes.
const (
	// ActionNotify wakes any waiter watching this task for this pattern.
	ActionNotify Action = "notify"
	// ActionRecord keeps a counter and the last matching line.
	ActionRecord Action = "record"
	// ActionKill ends the task. It is the only pattern action that does.
	ActionKill Action = "kill"
)

// Pattern is a caller's request to watch the output for a regular
// expression.
type Pattern struct {
	Name    string `json:"name"`
	Regex   string `json:"regex"`
	OnMatch Action `json:"on_match"`
}

// CompiledPattern is a Pattern with its regular expression built and its
// running state attached.
type CompiledPattern struct {
	Name    string
	Action  Action
	re      *regexp.Regexp
	count   int
	last    string
	groups  []string
	lastAt  time.Time
	matched bool
}

// PatternState is what task_status reports for one pattern.
type PatternState struct {
	Name  string `json:"name"`
	Count int    `json:"count"`

	// LastLine and Groups describe the most recent hit. Groups excludes the
	// whole match and is never nil, so a client can iterate it without
	// testing for JSON null first.
	LastLine string    `json:"last_line,omitempty"`
	Groups   []string  `json:"groups"`
	LastAt   time.Time `json:"last_at,omitzero"`
}

// CompilePatterns validates and builds a caller's pattern set.
//
// It rejects a duplicate name because the name is how task_status and a
// fired event identify which pattern hit. Two patterns sharing one would
// make that report ambiguous, and the caller could not tell which fired.
func CompilePatterns(pats []Pattern) ([]*CompiledPattern, error) {
	out := make([]*CompiledPattern, 0, len(pats))
	seen := make(map[string]bool, len(pats))

	for _, p := range pats {
		switch {
		case p.Name == "":
			return nil, fmt.Errorf("watch: a pattern needs a name")
		case p.Regex == "":
			return nil, fmt.Errorf("watch: pattern %q needs a regex", p.Name)
		}
		switch p.OnMatch {
		case ActionNotify, ActionRecord, ActionKill:
		default:
			return nil, fmt.Errorf(
				"watch: pattern %q has unknown on_match %q, want one of %q, %q, %q",
				p.Name, p.OnMatch, ActionNotify, ActionRecord, ActionKill)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("watch: duplicate pattern name %q", p.Name)
		}
		seen[p.Name] = true

		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("watch: cannot compile pattern %q: %w", p.Name, err)
		}
		out = append(out, &CompiledPattern{
			Name: p.Name, Action: p.OnMatch, re: re, groups: []string{},
		})
	}
	return out, nil
}
