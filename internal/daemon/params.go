// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"time"

	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/watch"
)

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

	// Patterns are evaluated against every complete line of output. They
	// are what keeps task_status cheap: a record pattern turns a log into
	// a counter and one line.
	Patterns []watch.Pattern `json:"patterns,omitempty"`
}

// StartResult is task_start's output.
type StartResult struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// WaitParams is task_wait's input.
//
// Until omitted means the default set: exit plus idle at 300 seconds. There
// is deliberately no absolute component in that default — silence catches a
// hung task, and elapsed time cannot tell one from a slow one.
type WaitParams struct {
	IDs     []string          `json:"ids"`
	Until   []watch.Condition `json:"until,omitempty"`
	Deliver string            `json:"deliver,omitempty"`
}

// WaitResult is task_wait's output.
//
// State and Exit describe the task at the moment the condition fired, which
// for every condition except exit means the task is still running and Exit
// is nil. A caller reads State before Exit: a task a signal ended has no
// meaningful exit code, and a zero there would read as success.
type WaitResult struct {
	ID     string   `json:"id"`
	Fired  string   `json:"fired"`
	Name   string   `json:"name,omitempty"`
	Line   string   `json:"line,omitempty"`
	Groups []string `json:"groups"`

	State string `json:"state"`
	Exit  *int   `json:"exit_code,omitempty"`

	// BlockedS is how long this call held the caller. It is the number the
	// warning quotes.
	BlockedS int `json:"blocked_s"`

	// Warning carries the long poll notice. It is never empty in this
	// plan: every wait blocks, and the agent needs to read that in the
	// tool result, where it makes its next decision.
	Warning string `json:"warning"`
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

	// Patterns reports each pattern's counter and last hit. It is never
	// nil, so a client can iterate it without testing for JSON null.
	Patterns []watch.PatternState `json:"patterns"`
}

// StatusResult is task_status's output.
type StatusResult struct {
	Tasks []StatusEntry `json:"tasks"`
}

// ReadParams is task_read's input. Since and Tail are mutually exclusive.
//
// A Tail read always reports Next as the end of the stream, even for a task
// still running: it is a snapshot of the last N lines, not a cursor
// resumable from the middle of one. A caller that follows a Tail read with a
// Since read receives nothing between the tail window and that call.
type ReadParams struct {
	ID       string `json:"id"`
	Since    *int64 `json:"since,omitempty"`
	Tail     *int   `json:"tail,omitempty"`
	MaxBytes int    `json:"max_bytes,omitempty"`
}

// ReadResult is task_read's output.
type ReadResult struct {
	Data           string `json:"data"`
	Next           int64  `json:"next"`
	TruncatedBytes int64  `json:"truncated_bytes"`
	EOF            bool   `json:"eof"`
}

// SearchParams is task_search's input.
type SearchParams struct {
	ID         string `json:"id"`
	Regex      string `json:"regex"`
	Context    int    `json:"context,omitempty"`
	MaxMatches int    `json:"max_matches,omitempty"`
}

// Match is one search hit with its surrounding lines.
//
// LineNumber counts from the first line still retained in the log, not from
// the first line the task ever printed: rotation can discard leading lines
// before a search ever runs, and nothing renumbers around the gap. Its JSON
// tag says so, because the wire name is what a client reads — a Go doc
// comment is invisible to it. A non-zero TruncatedBytes on the containing
// SearchResult means earlier lines are gone and this count does not include
// them; there is no way to recover their true line numbers, since the
// discarded newlines are gone with the bytes.
//
// Before and After carry no omitempty tag: a caller iterating "before" or
// "after" gets [] rather than a missing key when the window is empty, which
// at the default Context of 0 is every match.
type Match struct {
	LineNumber int      `json:"retained_line_number"`
	Line       string   `json:"line"`
	Before     []string `json:"before"`
	After      []string `json:"after"`
}

// SearchResult is task_search's output. More reports that MaxMatches hid
// further matches.
type SearchResult struct {
	Matches        []Match `json:"matches"`
	TruncatedBytes int64   `json:"truncated_bytes"`
	More           bool    `json:"more"`
}

// SignalParams is task_signal's input.
//
// The default is SIGTERM followed by SIGKILL after GraceS seconds, which is
// what a caller almost always wants: ask the task to stop, then insist.
type SignalParams struct {
	ID     string `json:"id"`
	Signal string `json:"signal,omitempty"`
	GraceS *int   `json:"grace_s,omitempty"`
}

// SignalResult is task_signal's output.
type SignalResult struct {
	ID     string `json:"id"`
	Signal string `json:"signal"`
}

// WriteParams is task_write's input.
type WriteParams struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

// WriteResult is task_write's output.
type WriteResult struct {
	Written int `json:"written"`
}

// defaultMaxOutput bounds one task's retained log when the caller sets no
// limit.
const defaultMaxOutput = 8 << 20

// defaultOnOutputCap is the only cap behavior Plan 2 implements.
const defaultOnOutputCap = "rotate"

const (
	defaultReadBytes  = 64 << 10
	defaultMaxMatches = 50
)

// maxResultBytes caps the log bytes one task_read result may carry.
//
// proto.MaxMessageBytes caps the whole encoded response, and JSON escaping
// can cost up to six characters per byte for control-heavy output, so the
// clamp sits an eighth under the protocol cap. Without it a tail read over a
// large retained log, or a max_bytes above the cap, builds a response
// WriteMessage refuses, and the client sees only "proto: decode: EOF".
//
// A since read resumes from Next, so the clamp only ever splits a read across
// more calls. A tail read reports the end of the stream either way, so the
// clamp narrows its window to the most recent bytes.
const maxResultBytes = proto.MaxMessageBytes / 8

// defaultGraceSeconds is how long a task has to exit on SIGTERM before
// taskd sends SIGKILL.
const defaultGraceSeconds = 10
