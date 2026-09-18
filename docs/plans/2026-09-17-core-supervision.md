# taskd Core Supervision Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run a child process under supervision, capture its output to a bounded log, and report a correct terminal state with an exit code.

**Architecture:** A `Runner` owns the child process and reports a `Result` built from `ProcessState`. Output flows into a `Store` that keeps an append-only log, serves reads by absolute byte cursor, and rotates when the log exceeds a cap. A clock seam makes time-based behavior testable without sleeping. The `taskd run` command wires these together in the foreground.

**Tech Stack:** Go 1.27, `github.com/creack/pty`, `golangci-lint` v2, `gofumpt`, GitHub Actions.

**Spec:** `docs/design/2026-09-17-taskd-design.md`

## Global Constraints

- Module path: `github.com/rcarback/taskd`. Go directive: `go 1.27`.
- Every Go file starts with `// SPDX-License-Identifier: AGPL-3.0-or-later`.
- Zero warnings. `make check` must pass with no findings before any commit.
- No test may call `time.Sleep` to wait for a time-based condition. Use the fake clock.
- Cursors are absolute byte offsets into the total output stream, never file offsets.
- `Written()` counts every byte ever produced. Rotation never decreases it.
- The five terminal states are exactly `exited`, `signaled`, `killed`, `failed`, `lost`.
- Commit messages use imperative mood, 72 characters or fewer in the subject. Never add attribution trailers.

---

### Task 1: Scaffold, toolchain, and continuous integration

**Files:**
- Create: `go.mod`, `Makefile`, `.golangci.yml`, `.github/workflows/ci.yml`
- Create: `internal/version/version.go`
- Test: `internal/version/version_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `make check` runs format, vet, lint, and race tests. `version.String() string`.

- [ ] **Step 1: Write the failing test**

```go
// internal/version/version_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package version

import "testing"

func TestStringIsNotEmpty(t *testing.T) {
	if got := String(); got == "" {
		t.Fatal("String() returned an empty string")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go mod init github.com/rcarback/taskd
go test ./internal/version/
```

Expected: FAIL, `undefined: String`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/version/version.go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version reports the build version of taskd.
package version

// Version is set at build time with -ldflags.
var Version = "dev"

// String returns the build version.
func String() string { return Version }
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/version/
```

Expected: PASS.

- [ ] **Step 5: Add the toolchain**

Create `.golangci.yml`:

```yaml
version: "2"
linters:
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
    - bodyclose
    - errorlint
    - gosec
    - misspell
    - revive
```

Create `Makefile`:

```make
GO ?= go

.PHONY: build test lint fmt vet check tools

tools:
	$(GO) get -tool mvdan.cc/gofumpt
	$(GO) get -tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint

fmt:
	$(GO) tool gofumpt -l -w .

vet:
	$(GO) vet ./...

lint:
	$(GO) tool golangci-lint run

test:
	$(GO) test -race ./...

build:
	$(GO) build -o bin/taskd ./cmd/taskd

check: vet lint test
	@test -z "$$($(GO) tool gofumpt -l .)" || { echo "gofumpt found unformatted files:"; $(GO) tool gofumpt -l .; exit 1; }
```

Create `.github/workflows/ci.yml`:

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7.0.1
      - uses: actions/setup-go@v7.0.0
        with:
          go-version: '1.27'
      - run: make tools
      - run: make check
```

- [ ] **Step 6: Verify the whole pipeline**

```bash
make tools
make check
actionlint
```

Expected: `make check` passes with no output from the format check. `actionlint` reports nothing.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum Makefile .golangci.yml .github/workflows/ci.yml internal/version/
git commit -m "Add Go scaffold, linters, and CI"
```

---

### Task 2: Clock seam

**Files:**
- Create: `internal/clock/clock.go`, `internal/clock/fake.go`
- Test: `internal/clock/fake_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `clock.Clock` interface with `Now() time.Time` and `After(time.Duration) <-chan time.Time`. `clock.System()` returns the real clock. `clock.NewFake(time.Time) *Fake` with `Advance(time.Duration)`.

Every later task that measures elapsed time or silence takes a `Clock`. Tests pass a `*Fake` and call `Advance`, so no test waits on real time.

- [ ] **Step 1: Write the failing test**

```go
// internal/clock/fake_test.go
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
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/clock/
```

Expected: FAIL, `undefined: NewFake`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/clock/clock.go
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
```

```go
// internal/clock/fake.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package clock

import (
	"sort"
	"sync"
	"time"
)

type waiter struct {
	at time.Time
	ch chan time.Time
}

// Fake is a Clock that only moves when Advance is called.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
}

// NewFake returns a Fake positioned at start.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now reports the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After returns a channel that receives once the fake clock reaches d from now.
func (f *Fake) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &waiter{at: f.now.Add(d), ch: make(chan time.Time, 1)}
	f.waiters = append(f.waiters, w)
	return w.ch
}

// Advance moves the clock forward and fires every timer that comes due.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	now := f.now

	sort.Slice(f.waiters, func(i, j int) bool { return f.waiters[i].at.Before(f.waiters[j].at) })

	var due, pending []*waiter
	for _, w := range f.waiters {
		if !w.at.After(now) {
			due = append(due, w)
			continue
		}
		pending = append(pending, w)
	}
	f.waiters = pending
	f.mu.Unlock()

	for _, w := range due {
		w.ch <- w.at
		close(w.ch)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race ./internal/clock/
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
make check
git add internal/clock/
git commit -m "Add a controllable clock for time-based tests"
```

---

### Task 3: Terminal escape stripping

**Files:**
- Create: `internal/ansi/ansi.go`
- Test: `internal/ansi/ansi_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `ansi.Strip(b []byte) []byte`, which removes terminal escape sequences and leaves every other byte untouched.

Tasks 4 and 6 call this before matching patterns and before returning text.

- [ ] **Step 1: Write the failing test**

```go
// internal/ansi/ansi_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package ansi

import "testing"

func TestStrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "hello world", "hello world"},
		{"colour codes", "\x1b[31merror:\x1b[0m boom", "error: boom"},
		{"cursor movement", "a\x1b[2Kb", "ab"},
		{"osc title with bell", "\x1b]0;my title\x07done", "done"},
		{"osc title with string terminator", "\x1b]0;t\x1b\\done", "done"},
		{"multibyte text survives", "café ✓ \U0001F600", "café ✓ \U0001F600"},
		{"multibyte next to escapes", "\x1b[32m✓\x1b[0m ok", "✓ ok"},
		{"newlines preserved", "one\ntwo\n", "one\ntwo\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Strip([]byte(tc.in))); got != tc.want {
				t.Fatalf("Strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripDoesNotAliasInput(t *testing.T) {
	in := []byte("\x1b[31mred\x1b[0m")
	out := Strip(in)
	out[0] = 'X'
	if in[5] == 'X' {
		t.Fatal("Strip returned a slice aliasing its input")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/ansi/
```

Expected: FAIL, `undefined: Strip`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/ansi/ansi.go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ansi removes terminal escape sequences from captured output.
package ansi

import "regexp"

// escape matches, in order: CSI sequences, OSC sequences ended by BEL or by
// the string terminator, and two-byte escape sequences. The pattern operates
// on bytes below 0x80 only, so multi-byte UTF-8 text passes through unchanged.
var escape = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` +
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
		`|\x1b[@-Z\\-_]`,
)

// Strip returns a copy of b with terminal escape sequences removed.
func Strip(b []byte) []byte {
	return escape.ReplaceAll(b, nil)
}
```

Note: `regexp.ReplaceAll` allocates a new slice, so the aliasing test passes without extra work.

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race ./internal/ansi/
```

Expected: PASS, all eight subtests.

- [ ] **Step 5: Commit**

```bash
make check
git add internal/ansi/
git commit -m "Add terminal escape stripping"
```

---

### Task 4: Output store with cursors and rotation

**Files:**
- Create: `internal/output/store.go`
- Test: `internal/output/store_test.go`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `output.Open(path string, maxBytes int64) (*Store, error)`
  - `(*Store).Append(p []byte) error`
  - `(*Store).ReadSince(cursor int64, limit int) (data []byte, next int64, truncated int64, err error)`
  - `(*Store).Tail(lines int) ([]byte, error)`
  - `(*Store).Written() int64`
  - `(*Store).Close() error`

`cursor` and `next` are absolute offsets into the total stream. `truncated` reports how many bytes rotation discarded between the requested cursor and the first byte still held. Task 7 calls `Append`, and Plan 2 calls the readers.

- [ ] **Step 1: Write the failing test**

```go
// internal/output/store_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package output

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "out.log"), maxBytes)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestReadSinceReturnsEachByteExactlyOnce(t *testing.T) {
	s := open(t, 1<<20)
	chunks := []string{"alpha\n", "beta\n", "gamma\n"}
	for _, c := range chunks {
		if err := s.Append([]byte(c)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var got bytes.Buffer
	cursor := int64(0)
	for {
		data, next, truncated, err := s.ReadSince(cursor, 4)
		if err != nil {
			t.Fatalf("ReadSince: %v", err)
		}
		if truncated != 0 {
			t.Fatalf("unexpected truncation of %d bytes", truncated)
		}
		if len(data) == 0 {
			break
		}
		got.Write(data)
		cursor = next
	}

	want := strings.Join(chunks, "")
	if got.String() != want {
		t.Fatalf("stream = %q, want %q", got.String(), want)
	}
	if cursor != int64(len(want)) {
		t.Fatalf("final cursor = %d, want %d", cursor, len(want))
	}
}

func TestWrittenCountsEveryByteAcrossRotation(t *testing.T) {
	s := open(t, 64)
	line := []byte("0123456789abcdef\n") // 17 bytes
	for range 20 {
		if err := s.Append(line); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	want := int64(len(line) * 20)
	if got := s.Written(); got != want {
		t.Fatalf("Written() = %d, want %d", got, want)
	}
}

func TestRotationBoundsTheFileAndReportsTruncation(t *testing.T) {
	s := open(t, 64)
	for range 20 {
		if err := s.Append([]byte("0123456789abcdef\n")); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	size, err := s.fileSize()
	if err != nil {
		t.Fatalf("fileSize: %v", err)
	}
	if size > 64 {
		t.Fatalf("file grew to %d bytes, cap is 64", size)
	}

	_, _, truncated, err := s.ReadSince(0, 1024)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if truncated == 0 {
		t.Fatal("ReadSince reported no truncation after rotation")
	}
}

func TestTailReturnsLastLines(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("one\ntwo\nthree\nfour\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := s.Tail(2)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if string(got) != "three\nfour\n" {
		t.Fatalf("Tail(2) = %q, want %q", got, "three\nfour\n")
	}
}

func TestReadSinceAtEndReturnsNothing(t *testing.T) {
	s := open(t, 1<<20)
	if err := s.Append([]byte("data\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, next, _, err := s.ReadSince(s.Written(), 1024)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected no data, got %q", data)
	}
	if next != s.Written() {
		t.Fatalf("next = %d, want %d", next, s.Written())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/output/
```

Expected: FAIL, `undefined: Open`.

- [ ] **Step 3: Write minimal implementation**

```go
// internal/output/store.go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package output stores task output in a bounded, append-only log and serves
// reads by absolute stream offset.
package output

import (
	"bytes"
	"fmt"
	"os"
	"sync"
)

// Store holds the output of one task.
//
// Callers address the log by absolute stream offset. Offsets keep counting
// across rotation, so a cursor stays meaningful after old bytes are dropped.
type Store struct {
	mu       sync.Mutex
	path     string
	file     *os.File
	maxBytes int64
	written  int64 // total bytes ever appended
	base     int64 // stream offset of the first byte still on disk
}

// Open creates or truncates the log at path. The file never exceeds maxBytes.
func Open(path string, maxBytes int64) (*Store, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("output: maxBytes must be positive, got %d", maxBytes)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("output: open %s: %w", path, err)
	}
	return &Store{path: path, file: f, maxBytes: maxBytes}, nil
}

// Append writes p to the log, rotating first if the log would exceed its cap.
func (s *Store) Append(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.file.Write(p); err != nil {
		return fmt.Errorf("output: write: %w", err)
	}
	s.written += int64(len(p))

	if s.written-s.base > s.maxBytes {
		if err := s.rotate(); err != nil {
			return err
		}
	}
	return nil
}

// rotate discards the oldest half of the retained log. The caller holds s.mu.
func (s *Store) rotate() error {
	keep := s.maxBytes / 2
	start := s.written - keep
	if start < s.base {
		start = s.base
	}

	buf := make([]byte, s.written-start)
	if _, err := s.file.ReadAt(buf, start-s.base); err != nil {
		return fmt.Errorf("output: read for rotation: %w", err)
	}
	if err := s.file.Truncate(0); err != nil {
		return fmt.Errorf("output: truncate: %w", err)
	}
	if _, err := s.file.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("output: rewrite after rotation: %w", err)
	}
	if _, err := s.file.Seek(int64(len(buf)), 0); err != nil {
		return fmt.Errorf("output: seek after rotation: %w", err)
	}
	s.base = start
	return nil
}

// Written reports the total number of bytes ever appended.
func (s *Store) Written() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

// ReadSince returns up to limit bytes starting at cursor. next is the cursor
// for the following call. truncated reports bytes that rotation discarded
// before the first byte returned.
func (s *Store) ReadSince(cursor int64, limit int) (data []byte, next int64, truncated int64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cursor < s.base {
		truncated = s.base - cursor
		cursor = s.base
	}
	if cursor >= s.written {
		return nil, s.written, truncated, nil
	}

	n := s.written - cursor
	if int64(limit) < n {
		n = int64(limit)
	}
	buf := make([]byte, n)
	if _, err := s.file.ReadAt(buf, cursor-s.base); err != nil {
		return nil, cursor, truncated, fmt.Errorf("output: read at %d: %w", cursor, err)
	}
	return buf, cursor + n, truncated, nil
}

// Tail returns the last n lines still held on disk.
func (s *Store) Tail(lines int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf := make([]byte, s.written-s.base)
	if _, err := s.file.ReadAt(buf, 0); err != nil {
		return nil, fmt.Errorf("output: read for tail: %w", err)
	}

	trimmed := bytes.TrimSuffix(buf, []byte("\n"))
	split := bytes.Split(trimmed, []byte("\n"))
	if lines < len(split) {
		split = split[len(split)-lines:]
	}
	out := bytes.Join(split, []byte("\n"))
	return append(out, '\n'), nil
}

// Close releases the underlying file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

// fileSize reports the size of the log on disk. Tests use it to check the cap.
func (s *Store) fileSize() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := s.file.Stat()
	if err != nil {
		return 0, fmt.Errorf("output: stat: %w", err)
	}
	return info.Size(), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race ./internal/output/
```

Expected: PASS, all five tests. If `TestTailReturnsLastLines` fails on a log with no trailing newline, fix `Tail`, not the test.

- [ ] **Step 5: Commit**

```bash
make check
git add internal/output/
git commit -m "Add bounded output store with cursor reads"
```

---

### Task 5: Supervisor with terminal states

**Files:**
- Create: `internal/supervisor/state.go`, `internal/supervisor/runner.go`
- Test: `internal/supervisor/runner_test.go`

**Interfaces:**
- Consumes: `clock.Clock` from Task 2
- Produces:
  - `supervisor.State` with constants `StateRunning`, `StateExited`, `StateSignaled`, `StateKilled`, `StateFailed`, `StateLost`
  - `supervisor.Result{State, ExitCode, Signal, Started, Ended, MaxRSSBytes}`
  - `supervisor.Spec{Command, Args, Dir, Env, PTY}`
  - `supervisor.Start(spec Spec, sink io.Writer, clk clock.Clock) (*Task, error)`
  - `(*Task).Wait() Result`
  - `(*Task).Signal(sig os.Signal) error`

Task 6 adds the PTY path behind `Spec.PTY`. Task 7 calls `Start` and `Wait`.

- [ ] **Step 1: Write the failing test**

```go
// internal/supervisor/runner_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
)

func TestExitZeroReportsExited(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "echo hello; exit 0"}}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if got.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", got.ExitCode)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("output = %q, want it to contain %q", out.String(), "hello")
	}
}

func TestNonZeroExitKeepsTheCode(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 42"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if got.ExitCode != 42 {
		t.Fatalf("ExitCode = %d, want 42", got.ExitCode)
	}
}

func TestSignalReportsSignaled(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "sleep 30"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := task.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}

	got := task.Wait()
	if got.State != StateSignaled {
		t.Fatalf("State = %q, want %q", got.State, StateSignaled)
	}
	if got.Signal != syscall.SIGTERM {
		t.Fatalf("Signal = %v, want %v", got.Signal, syscall.SIGTERM)
	}
}

func TestMissingBinaryReportsFailed(t *testing.T) {
	_, err := Start(Spec{Command: "this-binary-does-not-exist-9f1"}, &bytes.Buffer{}, clock.System())
	if err == nil {
		t.Fatal("Start returned no error for a missing binary")
	}
}

func TestResultRecordsATimeSpan(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 0"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := task.Wait()
	if got.Started.IsZero() || got.Ended.IsZero() {
		t.Fatalf("Started = %v, Ended = %v, want both set", got.Started, got.Ended)
	}
	if got.Ended.Before(got.Started) {
		t.Fatalf("Ended %v is before Started %v", got.Ended, got.Started)
	}
	if got.Ended.Sub(got.Started) > time.Minute {
		t.Fatalf("span of %v is implausible", got.Ended.Sub(got.Started))
	}
}

func TestWaitIsIdempotent(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 7"}}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := task.Wait()
	second := task.Wait()
	if first != second {
		t.Fatalf("Wait returned %+v then %+v, want the same result", first, second)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/supervisor/
```

Expected: FAIL, `undefined: Start`.

- [ ] **Step 3: Write the state type**

```go
// internal/supervisor/state.go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package supervisor owns task processes and reports how they ended.
package supervisor

import (
	"syscall"
	"time"
)

// State is the lifecycle state of a task.
type State string

// The task states. Every state except StateRunning is terminal.
const (
	StateRunning  State = "running"
	StateExited   State = "exited"
	StateSignaled State = "signaled"
	StateKilled   State = "killed"
	StateFailed   State = "failed"
	StateLost     State = "lost"
)

// Result describes how a task ended.
//
// Callers must read State before ExitCode. A task that ended on a signal has
// no meaningful exit code.
type Result struct {
	State       State
	ExitCode    int
	Signal      syscall.Signal
	Started     time.Time
	Ended       time.Time
	MaxRSSBytes int64
}
```

- [ ] **Step 4: Write the runner**

```go
// internal/supervisor/runner.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/rcarback/taskd/internal/clock"
)

// Spec describes a task to run.
type Spec struct {
	Command string
	Args    []string
	Dir     string
	Env     []string
	PTY     bool
}

// Task is a running child process owned by this process.
type Task struct {
	cmd     *exec.Cmd
	clk     clock.Clock
	started chan struct{}

	once   sync.Once
	result Result
	done   chan struct{}
}

// Start launches the task and copies its output into sink.
//
// Start returns an error only when the process fails to launch. Every other
// outcome appears in the Result from Wait.
func Start(spec Spec, sink io.Writer, clk clock.Clock) (*Task, error) {
	cmd := exec.Command(spec.Command, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdout = sink
	cmd.Stderr = sink

	t := &Task{cmd: cmd, clk: clk, done: make(chan struct{})}
	startedAt := clk.Now()

	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor: start %s: %w", spec.Command, err)
	}

	go t.reap(startedAt)
	return t, nil
}

// reap waits for the child and records the result exactly once.
func (t *Task) reap(startedAt time.Time) {
	_ = t.cmd.Wait()
	t.once.Do(func() {
		t.result = t.buildResult(startedAt)
		close(t.done)
	})
}

// Wait blocks until the task ends and returns the same Result on every call.
func (t *Task) Wait() Result {
	<-t.done
	return t.result
}

// Signal sends sig to the task. It is a no-op once the task has ended.
func (t *Task) Signal(sig os.Signal) error {
	if t.cmd.Process == nil {
		return fmt.Errorf("supervisor: task has no process")
	}
	if err := t.cmd.Process.Signal(sig); err != nil {
		return fmt.Errorf("supervisor: signal: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Write the result builder**

Append to `internal/supervisor/runner.go`, and add `"time"` to the import block:

```go
// buildResult converts the exit status into a Result.
//
// A task that ended on a signal reports StateSignaled and carries no
// meaningful exit code, so callers must read State first.
func (t *Task) buildResult(startedAt time.Time) Result {
	res := Result{Started: startedAt, Ended: t.clk.Now()}

	state := t.cmd.ProcessState
	if state == nil {
		res.State = StateFailed
		return res
	}

	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		res.State = StateSignaled
		res.Signal = ws.Signal()
	} else {
		res.State = StateExited
		res.ExitCode = state.ExitCode()
	}

	if ru, ok := state.SysUsage().(*syscall.Rusage); ok {
		res.MaxRSSBytes = maxRSSBytes(ru)
	}
	return res
}
```

- [ ] **Step 6: Handle the platform difference in peak memory**

`Rusage.Maxrss` is bytes on Darwin and kilobytes on Linux. Create two files so each platform reports bytes:

```go
// internal/supervisor/rusage_darwin.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import "syscall"

func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) }
```

```go
// internal/supervisor/rusage_linux.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import "syscall"

func maxRSSBytes(ru *syscall.Rusage) int64 { return int64(ru.Maxrss) * 1024 }
```

- [ ] **Step 7: Run tests to verify they pass**

```bash
go test -race ./internal/supervisor/
```

Expected: PASS, all six tests.

- [ ] **Step 8: Commit**

```bash
make check
git add internal/supervisor/
git commit -m "Add process supervision with terminal states"
```

---

### Task 6: Pseudo-terminal execution

**Files:**
- Modify: `internal/supervisor/runner.go`
- Create: `internal/supervisor/pty.go`
- Test: `internal/supervisor/pty_test.go`
- Modify: `go.mod`

**Interfaces:**
- Consumes: `Spec.PTY` from Task 5
- Produces: no new exported names. `Start` honours `Spec.PTY`.

The spec records the reason: the Codex terminal interface produced 6714 bytes on a pseudo-terminal and zero bytes without one.

- [ ] **Step 1: Write the failing test**

```go
// internal/supervisor/pty_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/clock"
)

// tty exits 0 when it runs on a terminal, and 1 otherwise.
const ttyProbe = "if [ -t 1 ]; then echo TTY; else echo PIPE; fi"

func TestPTYGivesTheChildATerminal(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", ttyProbe}, PTY: true}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := task.Wait(); got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if !strings.Contains(out.String(), "TTY") {
		t.Fatalf("output = %q, want it to contain TTY", out.String())
	}
}

func TestWithoutPTYTheChildSeesAPipe(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", ttyProbe}, PTY: false}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := task.Wait(); got.State != StateExited {
		t.Fatalf("State = %q, want %q", got.State, StateExited)
	}
	if !strings.Contains(out.String(), "PIPE") {
		t.Fatalf("output = %q, want it to contain PIPE", out.String())
	}
}

func TestPTYStillReportsTheExitCode(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 9"}, PTY: true}, &bytes.Buffer{}, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := task.Wait()
	if got.State != StateExited || got.ExitCode != 9 {
		t.Fatalf("Result = %+v, want exited with code 9", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/supervisor/ -run PTY
```

Expected: FAIL. `TestPTYGivesTheChildATerminal` reports `PIPE`, because `Spec.PTY` is still ignored.

- [ ] **Step 3: Add the dependency**

```bash
go get github.com/creack/pty@latest
```

- [ ] **Step 4: Write the pseudo-terminal path**

```go
// internal/supervisor/pty.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/creack/pty"
)

// startPTY launches cmd on a pseudo-terminal and copies its output to sink.
//
// The child sees a terminal, so it line-buffers its output instead of
// switching to block buffering. Standard output and standard error merge.
func startPTY(cmd *exec.Cmd, sink io.Writer) error {
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 120})
	if err != nil {
		return fmt.Errorf("supervisor: start on pty: %w", err)
	}

	go func() {
		defer func() { _ = ptmx.Close() }()
		// On Linux, reading the master after the child exits returns EIO.
		// That is the normal end of the stream, not a failure, so both it and
		// io.EOF end the copy quietly.
		_, _ = io.Copy(sink, ptmx)
	}()
	return nil
}
```

- [ ] **Step 5: Branch on Spec.PTY in Start**

In `internal/supervisor/runner.go`, replace these four lines from Task 5:

```go
	cmd.Stdout = sink
	cmd.Stderr = sink
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor: start %s: %w", spec.Command, err)
	}
```

with:

```go
	if spec.PTY {
		if err := startPTY(cmd, sink); err != nil {
			return nil, err
		}
	} else {
		cmd.Stdout = sink
		cmd.Stderr = sink
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("supervisor: start %s: %w", spec.Command, err)
		}
	}
```

`pty.StartWithSize` calls `cmd.Start` itself, so the non-PTY branch keeps the only other call.

- [ ] **Step 6: Run tests to verify they pass**

```bash
go test -race ./internal/supervisor/
```

Expected: PASS, all nine tests. If `TestPTYGivesTheChildATerminal` is flaky, the copy goroutine is racing the reap. Wait for the copy to finish before closing `done`.

- [ ] **Step 7: Commit**

```bash
make check
git add go.mod go.sum internal/supervisor/
git commit -m "Run tasks on a pseudo-terminal by default"
```

---

### Task 7: The `taskd run` command

**Files:**
- Create: `cmd/taskd/main.go`, `internal/taskdir/taskdir.go`
- Test: `internal/taskdir/taskdir_test.go`, `cmd/taskd/main_test.go`

**Interfaces:**
- Consumes: `supervisor.Start`, `output.Open`, `clock.System`
- Produces:
  - `taskdir.New(root string) (dir string, id string, err error)` creating `<root>/tasks/<id>/`
  - `taskd run [--no-pty] [--max-output BYTES] -- COMMAND [ARGS...]`

`run` supervises in the foreground, writes `out.log` into the task directory, prints the terminal state, and exits with the child's exit code. Plan 2 replaces the foreground behavior with the daemon.

- [ ] **Step 1: Write the failing test for the task directory**

```go
// internal/taskdir/taskdir_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package taskdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewCreatesAUniqueDirectory(t *testing.T) {
	root := t.TempDir()

	firstDir, firstID, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secondDir, secondID, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if firstID == secondID {
		t.Fatalf("two calls produced the same id %q", firstID)
	}
	for _, dir := range []string{firstDir, secondDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s has mode %o, want 700", dir, got)
		}
	}
	if filepath.Dir(firstDir) != filepath.Join(root, "tasks") {
		t.Fatalf("parent = %s, want %s", filepath.Dir(firstDir), filepath.Join(root, "tasks"))
	}
}

func TestIDsSortByCreationOrder(t *testing.T) {
	root := t.TempDir()
	_, first, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, second, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !(first < second) {
		t.Fatalf("ids %q and %q do not sort by creation order", first, second)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/taskdir/
```

Expected: FAIL, `undefined: New`.

- [ ] **Step 3: Write the task directory**

```go
// internal/taskdir/taskdir.go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package taskdir allocates the on-disk directory that holds one task.
package taskdir

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

var (
	mu   sync.Mutex
	last int64
)

// New creates <root>/tasks/<id>/ and returns the directory and the id.
//
// Ids carry a millisecond timestamp prefix, so they sort by creation order.
func New(root string) (string, string, error) {
	id, err := newID()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(root, "tasks", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("taskdir: create %s: %w", dir, err)
	}
	return dir, id, nil
}

// newID returns a sortable identifier that is unique within this process.
func newID() (string, error) {
	mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= last {
		ms = last + 1
	}
	last = ms
	mu.Unlock()

	var suffix [5]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("taskdir: read random bytes: %w", err)
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(suffix[:])
	return strconv.FormatInt(ms, 36) + "-" + enc, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test -race ./internal/taskdir/
```

Expected: PASS, both tests.

- [ ] **Step 5: Write the failing end-to-end test**

```go
// cmd/taskd/main_test.go
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReportsExitCodeAndWritesLog(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer

	code := run([]string{"--root", root, "--no-pty", "--", "sh", "-c", "echo marker; exit 3"}, &stdout)

	if code != 3 {
		t.Fatalf("run returned %d, want 3", code)
	}
	if !strings.Contains(stdout.String(), "exited") {
		t.Fatalf("stdout = %q, want it to mention the state", stdout.String())
	}

	logs, err := filepath.Glob(filepath.Join(root, "tasks", "*", "out.log"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("found %d logs, want 1", len(logs))
	}
	body, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(body), "marker") {
		t.Fatalf("log = %q, want it to contain marker", body)
	}
}

func TestRunRejectsAMissingCommand(t *testing.T) {
	if code := run([]string{"--root", t.TempDir(), "--"}, &bytes.Buffer{}); code == 0 {
		t.Fatal("run returned 0 for a missing command")
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

```bash
go test ./cmd/taskd/
```

Expected: FAIL, `undefined: run`.

- [ ] **Step 7: Write the command**

```go
// cmd/taskd/main.go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Command taskd supervises long-running tasks for coding agents.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/taskdir"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout))
}

// run supervises one command in the foreground and returns its exit code.
func run(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", defaultRoot(), "directory that holds task records")
	noPTY := fs.Bool("no-pty", false, "run without a pseudo-terminal")
	maxOutput := fs.Int64("max-output", 8<<20, "bytes of output to retain")

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}
	command := fs.Args()
	if len(command) == 0 {
		fmt.Fprintln(stdout, "taskd: no command given")
		return 2
	}

	dir, id, err := taskdir.New(*root)
	if err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	store, err := output.Open(filepath.Join(dir, "out.log"), *maxOutput)
	if err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	spec := supervisor.Spec{
		Command: command[0],
		Args:    command[1:],
		Env:     os.Environ(),
		PTY:     !*noPTY,
	}

	task, err := supervisor.Start(spec, storeWriter{store}, clock.System())
	if err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	res := task.Wait()
	fmt.Fprintf(stdout, "task %s %s exit=%d bytes=%d\n", id, res.State, res.ExitCode, store.Written())
	return res.ExitCode
}

// storeWriter adapts an output.Store to io.Writer.
type storeWriter struct{ s *output.Store }

func (w storeWriter) Write(p []byte) (int, error) {
	if err := w.s.Append(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// defaultRoot returns the directory that holds task records.
func defaultRoot() string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return ".taskd"
	}
	return filepath.Join(cache, "taskd")
}
```

- [ ] **Step 8: Run tests to verify they pass**

```bash
go test -race ./...
```

Expected: PASS across every package.

- [ ] **Step 9: Verify by hand**

```bash
make build
./bin/taskd run -- sh -c 'echo hello; sleep 1; echo done; exit 5'
echo "exit code: $?"
```

Expected: the command prints `hello` and `done`, then a line naming the task id with `exited` and `exit=5`, and the shell reports exit code 5.

- [ ] **Step 10: Commit**

```bash
make check
git add cmd/taskd/ internal/taskdir/
git commit -m "Add taskd run for foreground supervision"
```

---

## Done when

- `make check` passes with no findings.
- `go test -race ./...` passes.
- `./bin/taskd run -- sh -c 'exit 5'` exits 5 and writes `out.log` under the task root.
- A task run with a pseudo-terminal captures output that the same task without one does not.

## Not in this plan

The daemon, the socket protocol, autostart under `flock`, task metadata persistence, the `lost` state on daemon restart, patterns, wake conditions, subscriptions, and the remaining command verbs. Those belong to Plan 2.
