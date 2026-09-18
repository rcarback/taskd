# Wake Conditions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the wake-condition engine so `task_wait` answers, which lets an agent start a long job and go free instead of blocking in a poll loop.

**Architecture:** A tap wraps the existing output sink. Raw bytes still reach the log unchanged, and a stripped copy feeds line splitting, an idle timestamp, a line counter, and the regular expressions that start-time patterns and wait-time `match` conditions both use. A waiter composes those signals with the entry's `Done` channel and clock timers, and returns the first condition to fire. Delivery in this plan is the long poll: the daemon holds the connection open until a condition fires.

**Tech Stack:** Go 1.27, module `github.com/rcarback/taskd`. No new dependencies. Existing packages: `internal/ansi`, `internal/clock`, `internal/output`, `internal/supervisor`, `internal/daemon`, `internal/proto`, `internal/record`.

**Spec:** `docs/design/2026-09-17-taskd-design.md` — read the Wake conditions, Delivery, Tool surface, and Agent-facing text sections before Task 1.

## Global Constraints

- Every new file starts with the line `// SPDX-License-Identifier: AGPL-3.0-or-later`. The `goheader` linter fails the build without it.
- `time.Sleep` is forbidden by `forbidigo` in `.golangci.yml`. Every delay goes through `clock.Clock`. Tests use `clock.NewFake` and `Fake.BlockUntil(n)` before `Fake.Advance(d)`, so a timer registration can never race the advance.
- Wake conditions never kill a task. `elapsed` wakes the waiter and leaves the task running. Only `on_match: "kill"` and `kill_after_s` end a task, and both require the caller to opt in.
- With `until` omitted, the default is `exit` plus `idle` at 300 seconds. No absolute component applies by default.
- A nil slice marshals to JSON `null`, not `[]`. Every slice field that crosses the wire is allocated non-nil before it is returned. This defect appeared three times in the previous plan.
- No test in package `daemon` may call `t.Parallel()`. The package swaps the `saveRecord` variable, and parallel tests mutating it race.
- macOS limits `AF_UNIX` `sun_path` to 104 bytes, and `t.TempDir()` embeds the test name. Daemon tests that bind a socket use a short temp root, following the existing helper in `internal/daemon/daemon_test.go`.
- `make check` (vet, golangci-lint, test) passes before every commit.
- Run `go test ./... -race` for any task that adds concurrency.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/watch/condition.go` | Condition types, validation, defaults |
| `internal/watch/pattern.go` | Start-time pattern specs, compiled matchers, counters |
| `internal/watch/tap.go` | The output sink wrapper: strip, split lines, count, match |
| `internal/watch/wait.go` | Compose conditions into one first-to-fire wait |
| `internal/daemon/params.go` | `StartParams.Patterns`, `WaitParams`, `WaitResult` |
| `internal/daemon/verbs.go` | Tap construction in `start`, the `wait` handler, pattern counters in `status` |
| `internal/proto/proto.go` | `VerbWait` constant |
| `cmd/taskd/main.go` | The `wait` subcommand |

---

### Task 1: Wake condition types

**Files:**
- Create: `internal/watch/condition.go`
- Test: `internal/watch/condition_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `watch.Kind`, `watch.Condition`, `watch.Validate(conds []Condition) error`, `watch.Defaults() []Condition`, `watch.ParseUntil(s string) ([]Condition, error)`.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/watch"
)

func TestDefaultsAreExitAndIdle(t *testing.T) {
	got := watch.Defaults()
	want := []watch.Condition{
		{Type: watch.KindExit},
		{Type: watch.KindIdle, Seconds: 300},
	}
	if len(got) != len(want) {
		t.Fatalf("Defaults() returned %d conditions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Defaults()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDefaultsCarryNoAbsoluteComponent(t *testing.T) {
	// The spec is explicit: silence is the safety net, elapsed time is not.
	// An elapsed default would wake every agent on a slow-but-healthy build.
	for _, c := range watch.Defaults() {
		if c.Type == watch.KindElapsed {
			t.Fatalf("Defaults() includes %v: the default must have no absolute component", c)
		}
	}
}

func TestValidateRejectsBadConditions(t *testing.T) {
	cases := map[string]struct {
		cond watch.Condition
		want string
	}{
		"unknown type":      {watch.Condition{Type: "forever"}, "unknown condition type"},
		"idle without time": {watch.Condition{Type: watch.KindIdle}, "needs a positive seconds"},
		"negative elapsed":  {watch.Condition{Type: watch.KindElapsed, Seconds: -1}, "needs a positive seconds"},
		"match without re":  {watch.Condition{Type: watch.KindMatch}, "needs a pattern"},
		"bad regex":         {watch.Condition{Type: watch.KindMatch, Pattern: "(["}, "cannot compile"},
		"lines without n":   {watch.Condition{Type: watch.KindLines}, "needs a positive n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := watch.Validate([]watch.Condition{tc.cond})
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.cond)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsEveryKind(t *testing.T) {
	conds := []watch.Condition{
		{Type: watch.KindExit},
		{Type: watch.KindIdle, Seconds: 300},
		{Type: watch.KindElapsed, Seconds: 600},
		{Type: watch.KindMatch, Pattern: "error|panic", Name: "err"},
		{Type: watch.KindLines, N: 500},
	}
	if err := watch.Validate(conds); err != nil {
		t.Fatalf("Validate(every kind) = %v, want nil", err)
	}
}

func TestParseUntil(t *testing.T) {
	got, err := watch.ParseUntil("exit,idle:300,lines:50")
	if err != nil {
		t.Fatalf("ParseUntil: %v", err)
	}
	want := []watch.Condition{
		{Type: watch.KindExit},
		{Type: watch.KindIdle, Seconds: 300},
		{Type: watch.KindLines, N: 50},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseUntil returned %d conditions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ParseUntil()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseUntilEmptyGivesDefaults(t *testing.T) {
	got, err := watch.ParseUntil("")
	if err != nil {
		t.Fatalf("ParseUntil(\"\"): %v", err)
	}
	if len(got) != 2 || got[0].Type != watch.KindExit || got[1].Type != watch.KindIdle {
		t.Fatalf("ParseUntil(\"\") = %+v, want the defaults", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/`
Expected: FAIL — the package `internal/watch` does not exist.

- [ ] **Step 3: Write the implementation**

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -v`
Expected: PASS, every test.

- [ ] **Step 5: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add internal/watch/condition.go internal/watch/condition_test.go
git commit -m "watch: add wake condition types, validation, and parsing"
```

---

### Task 2: The output tap

**Files:**
- Create: `internal/watch/tap.go`
- Test: `internal/watch/tap_test.go`

**Interfaces:**
- Consumes: `watch.Kind` from Task 1. `ansi.Strip(b []byte) (clean []byte, pendingLen int)` and `clock.Clock` from the existing tree.
- Produces: `watch.Tap`, `watch.NewTap(sink io.Writer, clk clock.Clock) *Tap`, `(*Tap).Write`, `(*Tap).Lines() int64`, `(*Tap).LastWrite() time.Time`, `(*Tap).OnLines(n int64) (<-chan Event, func())`, `(*Tap).OnMatch(name string, re *regexp.Regexp) (<-chan Event, func())`, `watch.Event`.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"bytes"
	"regexp"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/watch"
)

func TestTapPassesRawBytesThrough(t *testing.T) {
	// The log must hold exactly what the task produced, escape sequences
	// and all. Only the matching view is stripped.
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	raw := []byte("\x1b[31merror:\x1b[0m boom\n")
	n, err := tap.Write(raw)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(raw) {
		t.Errorf("Write returned %d, want %d: a short count makes io.Copy report a failure", n, len(raw))
	}
	if got := sink.Bytes(); !bytes.Equal(got, raw) {
		t.Errorf("sink holds %q, want the raw bytes %q", got, raw)
	}
}

func TestTapCountsCompleteLinesOnly(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	if _, err := tap.Write([]byte("one\ntwo\nthr")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.Lines(); got != 2 {
		t.Errorf("Lines() = %d, want 2: the third line has no newline yet", got)
	}
	if _, err := tap.Write([]byte("ee\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.Lines(); got != 3 {
		t.Errorf("Lines() = %d, want 3 once the line completes", got)
	}
}

func TestTapMatchesAcrossAChunkBoundary(t *testing.T) {
	// A pattern that straddles two writes must still match. Splitting on
	// arrival rather than on lines is the classic way to miss it.
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error:`))
	defer cancel()

	if _, err := tap.Write([]byte("bui")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("fired early on a partial line: %+v", e)
	default:
	}

	if _, err := tap.Write([]byte("ld error: boom\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		if e.Kind != watch.KindMatch {
			t.Errorf("Kind = %q, want %q", e.Kind, watch.KindMatch)
		}
		if e.Name != "err" {
			t.Errorf("Name = %q, want %q", e.Name, "err")
		}
		if e.Line != "build error: boom" {
			t.Errorf("Line = %q, want the whole joined line", e.Line)
		}
	default:
		t.Fatal("no event: the pattern straddled the chunk boundary and was missed")
	}
}

func TestTapStripsEscapesBeforeMatching(t *testing.T) {
	// A colour code sitting inside the word must not stop the match.
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`^error: boom$`))
	defer cancel()

	if _, err := tap.Write([]byte("\x1b[31merror:\x1b[0m boom\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		if e.Line != "error: boom" {
			t.Errorf("Line = %q, want the stripped line", e.Line)
		}
	default:
		t.Fatal("no event: the escape sequences were not stripped before matching")
	}
}

func TestTapMatchFiresOnce(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error`))
	defer cancel()

	if _, err := tap.Write([]byte("error one\nerror two\nerror three\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := <-events; !ok {
		t.Fatal("channel closed without an event")
	}
	select {
	case e, ok := <-events:
		if ok {
			t.Fatalf("second event %+v: a waiter fires once and unregisters", e)
		}
	default:
		t.Fatal("channel still open after firing: the tap must close it so a waiter cannot block")
	}
}

func TestTapOnLinesFiresAtTheThreshold(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnLines(3)
	defer cancel()

	if _, err := tap.Write([]byte("a\nb\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		t.Fatalf("fired at 2 lines: %+v", e)
	default:
	}

	if _, err := tap.Write([]byte("c\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case e := <-events:
		if e.Kind != watch.KindLines {
			t.Errorf("Kind = %q, want %q", e.Kind, watch.KindLines)
		}
		if e.Count != 3 {
			t.Errorf("Count = %d, want 3", e.Count)
		}
	default:
		t.Fatal("no event at the threshold")
	}
}

func TestTapLastWriteTracksTheClock(t *testing.T) {
	start := time.Unix(1_000, 0)
	fake := clock.NewFake(start)
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, fake)

	if got := tap.LastWrite(); !got.Equal(start) {
		t.Errorf("LastWrite() = %v before any write, want the start time %v", got, start)
	}

	fake.Advance(30 * time.Second)
	if _, err := tap.Write([]byte("x\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := tap.LastWrite(), start.Add(30*time.Second); !got.Equal(want) {
		t.Errorf("LastWrite() = %v, want %v", got, want)
	}
}

func TestTapCancelUnregisters(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)))

	events, cancel := tap.OnMatch("err", regexp.MustCompile(`error`))
	cancel()

	if _, err := tap.Write([]byte("error here\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, ok := <-events; ok {
		t.Fatal("a cancelled waiter received an event")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -run TestTap`
Expected: FAIL — `undefined: watch.NewTap`.

- [ ] **Step 3: Write the implementation**

```go
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
```

- [ ] **Step 4: Run the tests, including the race detector**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -race -v`
Expected: PASS, every test, with a clean race report.

- [ ] **Step 5: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add internal/watch/tap.go internal/watch/tap_test.go
git commit -m "watch: add the output tap that counts lines and matches patterns"
```

---

### Task 3: Start-time patterns

**Files:**
- Create: `internal/watch/pattern.go`
- Test: `internal/watch/pattern_test.go`
- Modify: `internal/watch/tap.go` — add the pattern set to `NewTap` and evaluate it per line.

**Interfaces:**
- Consumes: `watch.Tap`, `watch.Event` from Task 2.
- Produces: `watch.Action`, `watch.Pattern`, `watch.PatternState`, `watch.CompilePatterns(pats []Pattern) ([]*CompiledPattern, error)`, `(*Tap).Stats() []PatternState`, `(*Tap).Killed() <-chan Event`. `NewTap` gains a third parameter: `NewTap(sink io.Writer, clk clock.Clock, pats []*CompiledPattern) *Tap`.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/watch"
)

func compile(t *testing.T, pats ...watch.Pattern) []*watch.CompiledPattern {
	t.Helper()
	got, err := watch.CompilePatterns(pats)
	if err != nil {
		t.Fatalf("CompilePatterns: %v", err)
	}
	return got
}

func TestCompilePatternsRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		pat  watch.Pattern
		want string
	}{
		"no name":      {watch.Pattern{Regex: "x", OnMatch: watch.ActionRecord}, "needs a name"},
		"no regex":     {watch.Pattern{Name: "a", OnMatch: watch.ActionRecord}, "needs a regex"},
		"bad regex":    {watch.Pattern{Name: "a", Regex: "([", OnMatch: watch.ActionRecord}, "cannot compile"},
		"bad action":   {watch.Pattern{Name: "a", Regex: "x", OnMatch: "explode"}, "unknown on_match"},
		"empty action": {watch.Pattern{Name: "a", Regex: "x"}, "unknown on_match"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := watch.CompilePatterns([]watch.Pattern{tc.pat})
			if err == nil {
				t.Fatalf("CompilePatterns(%+v) = nil, want an error", tc.pat)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCompilePatternsRejectsDuplicateNames(t *testing.T) {
	_, err := watch.CompilePatterns([]watch.Pattern{
		{Name: "err", Regex: "a", OnMatch: watch.ActionRecord},
		{Name: "err", Regex: "b", OnMatch: watch.ActionRecord},
	})
	if err == nil {
		t.Fatal("CompilePatterns accepted two patterns named err")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want it to name the duplicate", err)
	}
}

func TestRecordKeepsCountAndLastMatch(t *testing.T) {
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{
		Name: "progress", Regex: `(\d+)/(\d+) done`, OnMatch: watch.ActionRecord,
	})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	in := "1/400 done\nnoise\n142/400 done\n"
	if _, err := tap.Write([]byte(in)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	stats := tap.Stats()
	if len(stats) != 1 {
		t.Fatalf("Stats() returned %d entries, want 1", len(stats))
	}
	s := stats[0]
	if s.Name != "progress" {
		t.Errorf("Name = %q, want %q", s.Name, "progress")
	}
	if s.Count != 2 {
		t.Errorf("Count = %d, want 2", s.Count)
	}
	if s.LastLine != "142/400 done" {
		t.Errorf("LastLine = %q, want the most recent hit", s.LastLine)
	}
	want := []string{"142", "400"}
	if len(s.Groups) != len(want) {
		t.Fatalf("Groups = %v, want %v", s.Groups, want)
	}
	for i := range want {
		if s.Groups[i] != want[i] {
			t.Errorf("Groups[%d] = %q, want %q", i, s.Groups[i], want[i])
		}
	}
}

func TestStatsGroupsAreNeverNil(t *testing.T) {
	// A nil slice marshals to JSON null. A client iterating groups would
	// have to test for that, so an unmatched pattern reports an empty
	// slice instead.
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{Name: "err", Regex: "error:", OnMatch: watch.ActionRecord})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	stats := tap.Stats()
	if stats[0].Groups == nil {
		t.Error("Groups is nil for an unmatched pattern, want an empty slice")
	}
	if stats[0].Count != 0 {
		t.Errorf("Count = %d, want 0", stats[0].Count)
	}
}

func TestStatsIsNeverNil(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), nil)
	if tap.Stats() == nil {
		t.Error("Stats() is nil for a task with no patterns, want an empty slice")
	}
}

func TestKillActionReportsOnce(t *testing.T) {
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{
		Name: "oom", Regex: "out of memory", OnMatch: watch.ActionKill,
	})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	if _, err := tap.Write([]byte("fine\nout of memory\nout of memory again\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case e := <-tap.Killed():
		if e.Name != "oom" {
			t.Errorf("Name = %q, want %q", e.Name, "oom")
		}
		if e.Line != "out of memory" {
			t.Errorf("Line = %q, want the first hit", e.Line)
		}
	default:
		t.Fatal("no kill event: a kill pattern matched and nothing reported it")
	}

	select {
	case e, ok := <-tap.Killed():
		if ok {
			t.Fatalf("second kill event %+v: the channel must close after the first", e)
		}
	default:
		t.Fatal("the kill channel is still open: a second match would block the observer")
	}
}

func TestKillPatternStillRecordsItsCount(t *testing.T) {
	// A kill pattern is also a counter. An agent reading status after the
	// kill needs to know what ended the task.
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{Name: "oom", Regex: "out of memory", OnMatch: watch.ActionKill})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	if _, err := tap.Write([]byte("out of memory\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.Stats()[0].Count; got != 1 {
		t.Errorf("Count = %d, want 1", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -run 'TestCompile|TestRecord|TestStats|TestKill'`
Expected: FAIL — `undefined: watch.CompilePatterns`.

- [ ] **Step 3: Write `internal/watch/pattern.go`**

```go
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
```

- [ ] **Step 4: Extend the tap to evaluate patterns**

In `internal/watch/tap.go`, change the `Tap` struct, `NewTap`, and the per-line loop:

```go
// Add to the Tap struct, under mu:
	patterns []*CompiledPattern
	killed   chan Event
	killDone bool

// NewTap gains the pattern set. A nil or empty set is normal: most tasks
// run without patterns, and Stats then reports an empty slice.
func NewTap(sink io.Writer, clk clock.Clock, pats []*CompiledPattern) *Tap {
	return &Tap{
		sink: sink, clk: clk, lastWrite: clk.Now(),
		patterns: pats,
		killed:   make(chan Event, 1),
	}
}

// applyPatterns records every pattern that line matches, and reports the
// first kill. The caller holds mu.
func (t *Tap) applyPatterns(line string) {
	for _, p := range t.patterns {
		groups := p.re.FindStringSubmatch(line)
		if groups == nil {
			continue
		}
		p.count++
		p.last = line
		p.groups = append([]string{}, groups[1:]...)
		p.lastAt = t.lastWrite
		p.matched = true

		// A kill pattern counts like any other, so status still explains
		// what ended the task, but it reports only its first hit: the
		// channel holds one event and the task is ending regardless.
		if p.Action == ActionKill && !t.killDone {
			t.killDone = true
			t.killed <- Event{Kind: KindMatch, Name: p.Name, Line: line, Groups: p.groups}
			close(t.killed)
		}
	}
}

// Killed reports the first kill pattern to match. The channel closes after
// that event, or when the task ends without one having matched.
func (t *Tap) Killed() <-chan Event { return t.killed }

// Stats reports every pattern's counter and last hit.
//
// The result is never nil, and neither is any Groups field: a nil slice
// marshals to JSON null, which a client iterating it would have to test for.
func (t *Tap) Stats() []PatternState {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]PatternState, 0, len(t.patterns))
	for _, p := range t.patterns {
		s := PatternState{Name: p.Name, Count: p.count, Groups: append([]string{}, p.groups...)}
		if p.matched {
			s.LastLine = p.last
			s.LastAt = p.lastAt
		}
		out = append(out, s)
	}
	return out
}
```

Call `t.applyPatterns(line)` in the per-line loop inside `observe`, immediately before `t.matchLine(line)`, so a `notify` pattern and a wait-time `match` condition see the same line in the same pass.

- [ ] **Step 5: Run the tests**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -race -v`
Expected: PASS, every test including Task 2's.

- [ ] **Step 6: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add internal/watch/pattern.go internal/watch/pattern_test.go internal/watch/tap.go
git commit -m "watch: add start-time patterns with record, notify, and kill"
```

---

### Task 4: Compose conditions into one wait

**Files:**
- Create: `internal/watch/wait.go`
- Test: `internal/watch/wait_test.go`

**Interfaces:**
- Consumes: `watch.Condition`, `watch.Kind`, `watch.Tap`, `watch.Event` from Tasks 1-3. `clock.Clock`.
- Produces: `watch.Source` (interface), `watch.Wait(ctx context.Context, clk clock.Clock, srcs []Source, until []Condition) (Event, error)`.

- [ ] **Step 1: Write the failing test**

```go
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

func (f *fakeSource) ID() string             { return f.id }
func (f *fakeSource) Tap() *watch.Tap        { return f.tap }
func (f *fakeSource) Done() <-chan struct{}  { return f.done }

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
	select {
	case got := <-ch:
		t.Fatalf("fired after 200s of silence: %+v", got.e)
	default:
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -run TestWait`
Expected: FAIL — `undefined: watch.Wait`.

- [ ] **Step 3: Write the implementation**

```go
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
	defer cancel()

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

	// Releasing every armed condition before returning keeps a long-lived
	// task's tap from accumulating one registration per wait it ever
	// served. cancel above releases them; this waits for that to finish.
	defer wg.Wait()

	select {
	case e := <-fired:
		return e, nil
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
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
```

- [ ] **Step 4: Run the tests with the race detector**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/watch/ -race -count=20`
Expected: PASS. Use `-count=20` because this task adds concurrency, and a wake race that fails one run in twenty is the failure mode to catch here.

- [ ] **Step 5: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add internal/watch/wait.go internal/watch/wait_test.go
git commit -m "watch: compose conditions into one first-to-fire wait"
```

---

### Task 5: Wire the tap into the daemon

**Files:**
- Modify: `internal/daemon/params.go` — add `Patterns` to `StartParams`, add `Patterns` to the status result.
- Modify: `internal/daemon/registry.go` — add `AttachTap`/`Tap` to `Entry`.
- Modify: `internal/daemon/verbs.go` — build the tap in `start`, replace `storeWriter`, honour a kill pattern, report pattern counters in `status`.
- Test: `internal/daemon/watch_test.go`

**Interfaces:**
- Consumes: `watch.CompilePatterns`, `watch.NewTap`, `watch.Pattern`, `watch.PatternState`, `(*watch.Tap).Killed`, `(*watch.Tap).Stats` from Tasks 2-3.
- Produces: `(*Entry).AttachTap(*watch.Tap)`, `(*Entry).Tap() *watch.Tap`, `StartParams.Patterns []watch.Pattern`, `StatusResult.Patterns []watch.PatternState`.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/watch"
)

func TestStartRejectsABadPattern(t *testing.T) {
	d := newTestDaemon(t)
	_, err := callVerb(t, d, "task_start", StartParams{
		Command:  "true",
		Patterns: []watch.Pattern{{Name: "err", Regex: "([", OnMatch: watch.ActionRecord}},
	})
	if err == nil {
		t.Fatal("task_start accepted an uncompilable pattern")
	}
	if !strings.Contains(err.Error(), "cannot compile") {
		t.Errorf("error = %q, want it to name the compile failure", err)
	}
}

func TestStatusReportsPatternCounters(t *testing.T) {
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo 1/3 done; echo 2/3 done; echo 3/3 done"},
		Patterns: []watch.Pattern{
			{Name: "progress", Regex: `(\d+)/(\d+) done`, OnMatch: watch.ActionRecord},
		},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	e := entryFor(t, d, id)
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited", state)
	}

	got, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	tasks := got.(StatusResult).Tasks
	if len(tasks) != 1 {
		t.Fatalf("task_status returned %d tasks, want 1", len(tasks))
	}
	pats := tasks[0].Patterns
	if len(pats) != 1 {
		t.Fatalf("Patterns has %d entries, want 1", len(pats))
	}
	if pats[0].Count != 3 {
		t.Errorf("Count = %d, want 3", pats[0].Count)
	}
	if pats[0].LastLine != "3/3 done" {
		t.Errorf("LastLine = %q, want %q", pats[0].LastLine, "3/3 done")
	}
}

func TestPatternsFieldIsNeverNilOnTheWire(t *testing.T) {
	// A nil slice marshals to JSON null. A client iterating patterns would
	// have to test for that first.
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if got.(StatusResult).Tasks[0].Patterns == nil {
		t.Error("Patterns is nil for a task with no patterns, want an empty slice")
	}
}

func TestKillPatternEndsTheTask(t *testing.T) {
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo out of memory; sleep 60"},
		Patterns: []watch.Pattern{
			{Name: "oom", Regex: "out of memory", OnMatch: watch.ActionKill},
		},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	e := entryFor(t, d, id)
	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: the oom pattern asked for it", state)
	}
}

func TestOutputStillReachesTheLogThroughTheTap(t *testing.T) {
	// The tap sits between the process and the store. A regression here
	// would silently empty every task's log.
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo hello"},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	e := entryFor(t, d, id)
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited", state)
	}

	got, err := callVerb(t, d, "task_read", ReadParams{ID: id, Tail: intPtr(1)})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if !strings.Contains(got.(ReadResult).Data, "hello") {
		t.Errorf("Data = %q, want it to contain %q", got.(ReadResult).Data, "hello")
	}
}
```

Reuse the existing helpers in this package: `newTestDaemon`, `callVerb`, `entryFor`, `waitForState`, and `intPtr`. Read `internal/daemon/verbs_test.go` and `internal/daemon/read_test.go` for their signatures before writing the test. If `entryFor` or `intPtr` does not exist under that name, use whatever the package already provides rather than adding a duplicate.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/daemon/ -run 'TestStartRejectsABadPattern|TestStatusReportsPattern|TestKillPattern'`
Expected: FAIL — `StartParams` has no field `Patterns`.

- [ ] **Step 3: Add the fields**

In `internal/daemon/params.go`, add to `StartParams`:

```go
	// Patterns are evaluated against every complete line of output. They
	// are what keeps task_status cheap: a record pattern turns a log into
	// a counter and one line.
	Patterns []watch.Pattern `json:"patterns,omitempty"`
```

And to the per-task status struct:

```go
	// Patterns reports each pattern's counter and last hit. It is never
	// nil, so a client can iterate it without testing for JSON null.
	Patterns []watch.PatternState `json:"patterns"`
```

- [ ] **Step 4: Hold the tap on the entry**

In `internal/daemon/registry.go`, add to `Entry` under `mu`, following the pattern `AttachStore` already sets:

```go
	tap *watch.Tap

// AttachTap records the entry's output tap. It follows AttachStore: the tap
// exists before the process does, because it is the sink supervisor.Start
// writes into.
func (e *Entry) AttachTap(tap *watch.Tap) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tap = tap
}

// Tap returns the entry's output tap, or nil for a task this daemon did not
// start — one reconciled from a record left by a previous daemon.
func (e *Entry) Tap() *watch.Tap {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tap
}
```

Do not clear `tap` in `Finish`. The counters it holds are what `task_status` reports after the task ends, and the tap holds no file handle or process — only memory that dies with the entry.

- [ ] **Step 5: Build the tap in `start`**

In `internal/daemon/verbs.go`, inside `start`, after the output store opens and before `supervisor.Start`:

```go
	pats, err := watch.CompilePatterns(p.Patterns)
	if err != nil {
		_ = store.Close()
		_ = os.RemoveAll(dir)
		return StartResult{}, err
	}
	tap := watch.NewTap(storeWriter{store}, d.Clk, pats)
```

Then replace the sink argument to `supervisor.Start`:

```go
	}, tap, d.Clk)
```

After `e.AttachTask(task)`, add `e.AttachTap(tap)` and start the kill watcher:

```go
	e.AttachTap(tap)
	go d.awaitKillPattern(e, tap)
```

Add the watcher beside `enforceCap`:

```go
// awaitKillPattern ends the task when a kill pattern matches.
//
// It mirrors enforceCap: both end a task the client did not explicitly
// signal, and both run only because the caller opted in — enforceCap
// through kill_after_s, this through on_match: "kill".
func (d *Daemon) awaitKillPattern(e *Entry, tap *watch.Tap) {
	select {
	case <-e.Done():
	case _, ok := <-tap.Killed():
		if !ok {
			return // the channel closed without a match
		}
		task := e.LiveTask()
		if task == nil {
			return // it ended while the pattern was matching
		}
		e.RequestKill()
		_ = task.Signal(syscall.SIGKILL)
	}
}
```

- [ ] **Step 6: Report the counters in `status`**

Where `status` builds each task's entry, fill the new field from the tap, and never leave it nil:

```go
	pats := []watch.PatternState{}
	if tap := e.Tap(); tap != nil {
		pats = tap.Stats()
	}
```

- [ ] **Step 7: Run the tests**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/daemon/ -race -v`
Expected: PASS, every test including the ones that existed before this task.

- [ ] **Step 8: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add internal/daemon/ internal/watch/
git commit -m "daemon: run task output through the watch tap"
```

---

### Task 6: The task_wait verb

**Files:**
- Modify: `internal/proto/proto.go` — add `VerbWait`.
- Modify: `internal/daemon/params.go` — add `WaitParams` and `WaitResult`.
- Modify: `internal/daemon/verbs.go` — register and implement `wait`.
- Modify: `internal/daemon/daemon.go` — give a handler the connection's context.
- Test: `internal/daemon/wait_test.go`

**Interfaces:**
- Consumes: `watch.Wait`, `watch.Source`, `watch.Condition`, `watch.Event` from Task 4. `(*Entry).Tap` from Task 5.
- Produces: `proto.VerbWait`, `daemon.WaitParams`, `daemon.WaitResult`.

**Ruling carried from the spec:** `deliver: "notify"` has no delivery path until the adapters of the next plan exist. The spec says a harness with no notification path falls back to block and emits the long-poll warning, and that it must not fail, because a failure would teach the agent to stop asking for notification. This plan implements `block`, and treats `notify` as a request that degrades to `block` with the warning attached.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/watch"
)

func TestWaitReturnsWhenTheTaskExits(t *testing.T) {
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{
		IDs:   []string{id},
		Until: []watch.Condition{{Type: watch.KindExit}},
	})
	if err != nil {
		t.Fatalf("task_wait: %v", err)
	}
	w := got.(WaitResult)
	if w.ID != id {
		t.Errorf("ID = %q, want %q", w.ID, id)
	}
	if w.Fired != string(watch.KindExit) {
		t.Errorf("Fired = %q, want %q", w.Fired, watch.KindExit)
	}
	if w.State != "exited" {
		t.Errorf("State = %q, want exited", w.State)
	}
	if w.Exit == nil || *w.Exit != 0 {
		t.Errorf("Exit = %v, want 0", w.Exit)
	}
}

func TestWaitAlwaysCarriesTheLongPollWarning(t *testing.T) {
	// The warning is the product. A blocked agent is the problem this tool
	// exists to remove, so every blocking wait says so where the model
	// reads it.
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{id}, Deliver: "block"})
	if err != nil {
		t.Fatalf("task_wait: %v", err)
	}
	if w := got.(WaitResult); !strings.Contains(w.Warning, "LONG-POLL") {
		t.Errorf("Warning = %q, want the long poll warning", w.Warning)
	}
}

func TestWaitNotifyDegradesRatherThanFails(t *testing.T) {
	// A failure would teach the agent to stop asking for notification.
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{id}, Deliver: "notify"})
	if err != nil {
		t.Fatalf("task_wait with deliver notify: %v, want a degraded success", err)
	}
	w := got.(WaitResult)
	if !strings.Contains(w.Warning, "Notification delivery unavailable") {
		t.Errorf("Warning = %q, want it to say notification was unavailable", w.Warning)
	}
	if w.Fired != string(watch.KindExit) {
		t.Errorf("Fired = %q, want the wait to have completed anyway", w.Fired)
	}
}

func TestWaitDefaultsToExitAndIdle(t *testing.T) {
	d := newTestDaemon(t)
	res, err := callVerb(t, d, "task_start", StartParams{Command: "true"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := res.(StartResult).ID

	got, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_wait with no until: %v", err)
	}
	if w := got.(WaitResult); w.Fired != string(watch.KindExit) {
		t.Errorf("Fired = %q, want exit from the default condition set", w.Fired)
	}
}

func TestWaitRejectsAnUnknownID(t *testing.T) {
	d := newTestDaemon(t)
	_, err := callVerb(t, d, "task_wait", WaitParams{IDs: []string{"nosuchtask"}})
	if err == nil {
		t.Fatal("task_wait accepted an unknown id")
	}
	if !strings.Contains(err.Error(), "nosuchtask") {
		t.Errorf("error = %q, want it to name the unknown id", err)
	}
}

func TestWaitRejectsNoIDs(t *testing.T) {
	d := newTestDaemon(t)
	_, err := callVerb(t, d, "task_wait", WaitParams{})
	if err == nil {
		t.Fatal("task_wait with no ids returned nil, want an error")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./internal/daemon/ -run TestWait`
Expected: FAIL — `undefined: WaitParams`.

- [ ] **Step 3: Add the verb constant**

In `internal/proto/proto.go`, add to the constant block and update the comment above it, which currently reads `// The verbs this plan implements. Plan 3 adds task_wait.`:

```go
// The verbs the daemon implements.
const (
	VerbStart  Verb = "task_start"
	VerbWait   Verb = "task_wait"
	VerbStatus Verb = "task_status"
	VerbRead   Verb = "task_read"
	VerbSearch Verb = "task_search"
	VerbSignal Verb = "task_signal"
	VerbWrite  Verb = "task_write"
)
```

- [ ] **Step 4: Add the parameter types**

In `internal/daemon/params.go`:

```go
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
	ID     string `json:"id"`
	Fired  string `json:"fired"`
	Name   string `json:"name,omitempty"`
	Line   string `json:"line,omitempty"`
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
```

- [ ] **Step 5: Give the handler a context**

`Handler` is `func(params json.RawMessage) (any, error)`, which gives `wait` no way to notice a client that hung up. Widen it to carry the connection's context:

```go
// Handler answers one verb. ctx ends when the client disconnects or the
// daemon shuts down, which is what releases a long poll that would
// otherwise hold a connection goroutine forever.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)
```

Update `jsonHandler` and every existing handler registration to match. The existing handlers ignore the context: give them an `_ context.Context` parameter rather than a second adapter, so there is one handler shape rather than two.

In `daemon.go`, the connection loop already closes every open connection when `Serve`'s context ends. Its doc comment says so, and names a handler that holds a connection open as a future long poll will. Derive a per-connection context there and pass it to `dispatch`.

- [ ] **Step 6: Implement the handler**

In `internal/daemon/verbs.go`, register it in `Register` between start and status:

```go
	d.Handle(proto.VerbWait, jsonHandler(d.wait))
```

```go
// entrySource adapts an Entry to watch.Source.
type entrySource struct{ e *Entry }

func (s entrySource) ID() string            { return s.e.Record().ID }
func (s entrySource) Tap() *watch.Tap       { return s.e.Tap() }
func (s entrySource) Done() <-chan struct{} { return s.e.Done() }

// wait blocks until a condition fires on one of the named tasks.
func (d *Daemon) wait(ctx context.Context, p WaitParams) (WaitResult, error) {
	if len(p.IDs) == 0 {
		return WaitResult{}, fmt.Errorf("daemon: task_wait needs at least one id")
	}

	srcs := make([]watch.Source, 0, len(p.IDs))
	for _, id := range p.IDs {
		e, ok := d.Reg.Get(id)
		if !ok {
			return WaitResult{}, fmt.Errorf("daemon: no task %q", id)
		}
		if e.Tap() == nil {
			// A task reconciled from a previous daemon's record has no
			// live tap, so nothing can observe its output. Exit is the
			// only condition that could still fire, and the record
			// already says it ended. Reading status is the honest answer.
			return WaitResult{}, fmt.Errorf(
				"daemon: task %q is not owned by this daemon, so nothing watches its output; read task_status instead", id)
		}
		srcs = append(srcs, entrySource{e})
	}

	started := d.Clk.Now()
	e, err := watch.Wait(ctx, d.Clk, srcs, p.Until)
	if err != nil {
		return WaitResult{}, err
	}
	blocked := int(d.Clk.Now().Sub(started).Seconds())

	fired, _ := d.Reg.Get(e.TaskID)
	rec := fired.Record()

	return WaitResult{
		ID:       e.TaskID,
		Fired:    string(e.Kind),
		Name:     e.Name,
		Line:     e.Line,
		Groups:   append([]string{}, e.Groups...),
		State:    string(rec.State),
		Exit:     rec.Exit,
		BlockedS: blocked,
		Warning:  longPollWarning(p.Deliver, blocked),
	}, nil
}

// longPollWarning states the cost the caller just paid.
//
// It is never empty. Every wait in this plan blocks, and the agent decides
// what to do next from the tool result, so the cost belongs there rather
// than in a log the agent never reads.
func longPollWarning(deliver string, blockedS int) string {
	w := fmt.Sprintf("LONG-POLL: blocked this session for %ds.", blockedS)
	if deliver == "notify" {
		w += " Notification delivery unavailable here: no wake adapter is registered."
	}
	return w + " You could not answer questions or compact while blocked."
}
```

If `Registry` has no `Get(id)` method, add one beside `Add` and `List`, honouring the lock order the registry's doc comment states: `Registry.mu` is always taken before `Entry.mu`, never the other way round.

- [ ] **Step 7: Run the tests**

Run: `cd /Users/carback1/Code/taskd && go test ./... -race`
Expected: PASS. The handler signature change touches every verb, so the whole tree must build and pass, not only this package.

- [ ] **Step 8: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add internal/proto/ internal/daemon/
git commit -m "daemon: add task_wait with long poll delivery"
```

---

### Task 7: The taskd wait subcommand

**Files:**
- Modify: `cmd/taskd/main.go` — add the `wait` subcommand and its usage line.
- Test: `cmd/taskd/main_test.go`

**Interfaces:**
- Consumes: `proto.VerbWait`, `daemon.WaitParams`, `daemon.WaitResult` from Task 6. `watch.ParseUntil` from Task 1. `client.Call(root string, req proto.Request) (proto.Response, error)`.
- Produces: the `taskd wait` command line.

**Why this task matters:** this is the whole Claude Code integration. The spec's host table lists Claude Code's wake path as "`taskd wait` as a background command" with "None" under new code, because the harness already notifies when a background command exits. Without this subcommand there is no Claude Code support.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestWaitUsageOnNoID(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"wait"}, &out); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "--id") {
		t.Errorf("output = %q, want it to name the missing flag", out.String())
	}
}

func TestWaitRejectsABadUntil(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch([]string{"wait", "--id", "abc", "--until", "forever"}, &out); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "unknown condition type") {
		t.Errorf("output = %q, want it to name the bad condition", out.String())
	}
}

func TestUsageNamesWait(t *testing.T) {
	var out bytes.Buffer
	if code := dispatch(nil, &out); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(out.String(), "taskd wait") {
		t.Errorf("usage = %q, want it to list the wait subcommand", out.String())
	}
}
```

Add an end-to-end test in `internal/daemon` that starts a real daemon, starts a task, and calls the wait verb over the socket, rather than driving the binary from here. `cmd/taskd` tests stay at the argument-parsing level, matching how `run` and `serve` are already tested in this file.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/carback1/Code/taskd && go test ./cmd/taskd/ -run TestWait`
Expected: FAIL — dispatch prints usage and returns 2 for an unknown subcommand, so the `--id` assertion fails.

- [ ] **Step 3: Implement the subcommand**

In `cmd/taskd/main.go`, extend `usage` and `dispatch`:

```go
const usage = "usage: taskd run [flags] -- COMMAND [ARGS...]\n" +
	"       taskd wait --id ID [--id ID...] [--until exit,idle:300]\n" +
	"       taskd serve [--root DIR]"
```

```go
	case "wait":
		return wait(args[1:], stdout)
```

```go
// idList collects a repeated --id flag.
type idList []string

func (l *idList) String() string { return strings.Join(*l, ",") }

func (l *idList) Set(v string) error {
	if v == "" {
		return errors.New("an id cannot be empty")
	}
	*l = append(*l, v)
	return nil
}

// wait blocks until a condition fires and prints the result as JSON.
//
// This is the Claude Code wake path. The agent runs it as a background
// command and the harness notifies the session when it exits, so taskd
// needs no delivery code of its own for that harness.
func wait(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var ids idList
	fs.Var(&ids, "id", "task id to wait on; repeat for several")
	until := fs.String("until", "", "conditions, for example exit,idle:300 (default exit,idle:300)")
	root := fs.String("root", paths.Root(), "directory that holds task records")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n%s\n", err, usage)
		return 2
	}
	if len(ids) == 0 {
		_, _ = fmt.Fprintf(stdout, "taskd: wait needs at least one --id\n%s\n", usage)
		return 2
	}

	conds, err := watch.ParseUntil(*until)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	params, err := json.Marshal(daemon.WaitParams{IDs: ids, Until: conds, Deliver: "block"})
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	resp, err := client.Call(*root, proto.Request{Verb: proto.VerbWait, Params: params})
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	if !resp.OK {
		_, _ = fmt.Fprintf(stdout, "taskd: %s\n", resp.Error)
		return 1
	}

	// The raw result goes to stdout so the agent reads the same fields the
	// socket carried, including the long poll warning.
	_, _ = fmt.Fprintf(stdout, "%s\n", resp.Result)
	return 0
}
```

Check `proto.Response`'s field names before writing this: the handler returns `Result` as encoded JSON, and the exact field and type must match what `internal/proto/proto.go` declares.

- [ ] **Step 4: Run the tests**

Run: `cd /Users/carback1/Code/taskd && go test ./... -race`
Expected: PASS.

- [ ] **Step 5: Verify by hand, end to end**

```bash
cd /Users/carback1/Code/taskd
go build -o bin/taskd ./cmd/taskd
./bin/taskd run -- sh -c 'echo start; sleep 3; echo done'
```

Then in one shell start a long task and in another wait on it, confirming the wait returns when the task exits and prints a `LONG-POLL` warning.

- [ ] **Step 6: Update the skill and the README**

`skills/task-monitor/SKILL.md` describes `deliver: "notify"` as the normal path. In this plan every wait blocks, so correct the two places that would mislead an agent:

In the Workflow section, replace step 2 and step 3 with the blocking reality, and in the "Blocking on purpose" section say that `block` is currently the only delivery mode. Keep the tool table as it is: all seven tools now exist.

In `README.md`, the host support table lists Claude Code as `Works today`. That is now true rather than planned, so leave it. Change the Pi and Codex rows to say the adapter is not built yet, so the table describes what ships rather than what is designed.

Run `vale skills/task-monitor/SKILL.md README.md` and fix the findings.

- [ ] **Step 7: Run the gate and commit**

```bash
cd /Users/carback1/Code/taskd
make check
git add cmd/taskd/ skills/ README.md
git commit -m "cmd: add taskd wait, the Claude Code wake path"
```

---

## Out of scope, and why

Record these in `docs/known-issues.md` at the end of the plan rather than building them here.

- **Notification delivery.** `deliver: "notify"` degrades to a long poll. Real delivery needs a registered wake adapter, which is the next plan. The spec's registration safety rules (template names, never a command string, a session may register only for itself) belong with it.
- **A killing output cap.** `start` still rejects `on_output_cap` set to `kill`. Its current error message says that such a cap needs the overflow signal that arrives with pattern support. This plan adds pattern support, so the message is now stale. Either build the cap or reword the error. Do not leave it naming a blocker that no longer exists.
- **Pattern counters do not survive a daemon restart.** The tap lives in memory. A task reconciled from a previous daemon's record reports an empty pattern list, and `task_wait` on it returns an error naming that. Persisting counters into `meta.json` is a separate change.
- **Context threshold text.** The spec's Agent-facing text section specifies a compaction prompt in the `task_wait` result, with a model-aware threshold table. That text belongs with real notification, because its first line tells the agent it is subscribed and free until the condition fires. A long poll makes that claim false.

---

## Self-Review

**Spec coverage.** The spec's "Wake conditions" section names five condition types: Task 1 defines all five, Task 2 implements `match` and `lines` on the output path, and Task 4 implements `exit`, `idle`, and `elapsed`. The `until`-omitted default is Task 1 and is asserted in Tasks 1, 4, and 6. Several ids in one call is Task 4. "Wake conditions never kill a task" is asserted in Task 4. The "Delivery" section's long poll and its warning text are Task 6, and the notify fallback is Task 6 with its ruling stated. `task_start`'s `patterns` field with three `on_match` actions is Task 3, wired in Task 5, with the `record` counter reaching `task_status` in Task 5. The "Wake adapters" table's Claude Code row is Task 7.

**Gaps, stated rather than hidden.** The preceding Out of scope section lists four: daemon-side notification delivery, the Pi and Codex adapters, the agent-facing subscription text, and a killing output cap. The spec covers all four. This plan covers none of them, by choice.

**Type consistency.** `NewTap` gains its third parameter in Task 3 and every later call site passes it. `Event` is defined once in Task 2 and extended with no field renames. `watch.Source` in Task 4 is implemented by `entrySource` in Task 6 with matching method names. `Handler` changes shape in Task 6, and that task says every existing registration changes with it.
