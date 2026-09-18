// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import "time"

// StartParams is task_start's input.
//
// PTY is a pointer so an omitted field means "use the default", which is
// true, rather than false.
//
// KillAfterS is nil for "no cap": the task runs until it ends on its own or
// a client signals it. A non-nil value must be positive; a caller sending 0
// or a negative number gets an error naming the field rather than a cap
// that fires on its very next tick, which would kill the task before the
// caller had any real chance to observe it running.
type StartParams struct {
	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	Cwd         string   `json:"cwd,omitempty"`
	Name        string   `json:"name,omitempty"`
	PTY         *bool    `json:"pty,omitempty"`
	KillAfterS  *int     `json:"kill_after_s"`
	OnOutputCap string   `json:"on_output_cap,omitempty"`
	Harness     string   `json:"harness,omitempty"`
	Session     string   `json:"session,omitempty"`
	MaxOutput   int64    `json:"max_output,omitempty"`
}

// StartResult is task_start's output.
type StartResult struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// StatusParams is task_status's input. With no IDs it lists.
type StatusParams struct {
	IDs     []string `json:"ids,omitempty"`
	Session string   `json:"session,omitempty"`
	All     bool     `json:"all,omitempty"`
}

// StatusEntry is one task's terse state.
//
// Exit is nil unless State is exited, so a caller cannot read a zero as
// success for a task that a signal ended.
type StatusEntry struct {
	ID        string     `json:"id"`
	Name      string     `json:"name,omitempty"`
	State     string     `json:"state"`
	Command   string     `json:"command"`
	Exit      *int       `json:"exit_code,omitempty"`
	Signal    string     `json:"signal,omitempty"`
	Session   string     `json:"session,omitempty"`
	Written   int64      `json:"written"`
	Retained  int64      `json:"retained"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	OutputErr string     `json:"output_err,omitempty"`
}

// StatusResult is task_status's output.
type StatusResult struct {
	Tasks []StatusEntry `json:"tasks"`
}

// defaultMaxOutput bounds one task's retained log when the caller sets no
// limit.
const defaultMaxOutput = 8 << 20

// defaultOnOutputCap is the only cap behavior Plan 2 implements.
const defaultOnOutputCap = "rotate"
