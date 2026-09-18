# Daemon and Protocol Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the taskd daemon, its socket protocol, and the six task verbs that need no wake logic, so an agent can start a task, read its output, and control it across sessions.

**Architecture:** One daemon per user per machine owns every task and answers a Unix socket. A client connects, and starts the daemon itself if the socket is missing or stale, guarded by an exclusive lock so concurrent clients start exactly one. Each task keeps a directory holding its metadata and its raw log. The daemon holds live tasks in memory and writes a record to disk at every state change, so a status read after a daemon restart still reports the truth.

**Tech Stack:** Go 1.27, standard library only. No new dependencies. The existing `internal/supervisor`, `internal/output`, `internal/clock`, `internal/ansi`, and `internal/taskdir` packages from Plan 1 supply process ownership, the bounded log, the injectable clock, escape stripping, and directory allocation.

**Spec:** `docs/design/2026-09-17-taskd-design.md`

**Predecessor:** `docs/plans/2026-09-17-core-supervision.md` (complete, merged at `acc0790`)

## Global Constraints

- Module path: `github.com/rcarback/taskd`. Go directive: `go 1.27`.
- Every Go file starts with `// SPDX-License-Identifier: AGPL-3.0-or-later` and nothing above it. `goheader` enforces this; `make check` fails otherwise.
- No test may call `time.Sleep`. `forbidigo` enforces this. Use `clock.Fake`.
- Zero warnings. `make check` must pass with no findings before any commit.
- No new third-party dependency. If a task appears to need one, stop and report it.
- The five terminal states are exactly `exited`, `signaled`, `killed`, `failed`, `lost`.
- `Result.ExitCode` says nothing unless `State` is `exited`. Every reader guards on state first. Three separate defects in Plan 1 came from ignoring this.
- Cursors are absolute byte offsets into the total output stream, never file offsets.
- Wake conditions and caps never kill a task. `kill_after_s` defaults to null. Only `task_signal` and an explicitly opted-in cap may end a task.
- Every response that omits bytes reports how many. Silent truncation is how an agent concludes a failed build succeeded.
- The socket and every file taskd creates are mode 0600. Directories are 0700.
- Commit messages use imperative mood, 72 characters or fewer in the subject. Never add attribution trailers.

## Decisions This Plan Makes

These resolve gaps or correct errors in the spec. Each is listed so a reviewer can reject it before execution rather than discover it in a diff.

**1. `setsid` is a syscall, not a binary.** The spec describes the client as re-execing itself under `setsid`. macOS ships no `setsid` executable, so that mechanism works on Linux and fails on Darwin. Task 5 uses `syscall.SysProcAttr{Setsid: true}`, which both platforms support natively and which needs no external program.

**2. `ansi.Strip` changes signature to report an incomplete trailing sequence.** Plan 1 parked a defect: a truncated escape sequence cannot be handled by a pure `Strip([]byte) []byte` when output arrives in chunks. Task 2 changes it to `Strip(p []byte) (clean []byte, pendingLen int)`. `pendingLen` counts trailing bytes that begin a sequence the buffer does not finish. A cursor read rewinds its `next` cursor by `pendingLen`, so the partial sequence is re-read with its continuation. Tail and search drop it. This needs no stateful stripper and no per-reader state, and it fixes the parked OSC defect as a consequence.

**3. The `index` file is deferred to Plan 3.** The spec's task record lists `index` holding byte offsets for cursor reads. `output.Store` already serves cursor reads from absolute offsets held in memory, and a Plan 2 task does not survive a daemon restart. The file would be written and never read. Plan 3 adds it when search across a restart needs it.

**4. `task_start` does not accept `patterns`.** Pattern matching belongs to Plan 3. A parameter that the daemon accepts and ignores is worse than one that arrives later.

**5. One request and one response per connection.** The client writes a single JSON request line, reads a single JSON response line, and closes. This needs no request multiplexing and no correlation ids, and it already supports Plan 3's long poll: a held-open connection is a response that has not arrived yet.

**6. The daemon runs until it is killed.** The spec asks for no idle shutdown, and adding one would race every client that is about to connect.

**7. This plan builds no `taskd __child` subcommand.** The spec's process model lists it as an internal re-exec target, and nothing needs it: the client re-execs the binary as `taskd serve`, and the supervisor spawns task processes directly through `os/exec`, which needs no shim of our own. A third subcommand that nothing calls is dead surface. If Plan 3 finds a use, it adds one then.

**8. The daemon filters by session only when asked, and the adapter always asks.** The spec gives `task_status` a default of the current session, and requires a caller to ask for every task explicitly. A daemon cannot know which session a client belongs to unless the client says so, so that default belongs to the tool surface rather than to the daemon. `StatusParams.Session` filters when set, and Plan 4's adapters set it on every call. `All: true` is the explicit request for every task, and it is what a person at the command line uses.

## File Structure

| File | Responsibility |
|------|----------------|
| `internal/proto/proto.go` | Request and response types, verb names, error shape |
| `internal/proto/codec.go` | Newline-delimited JSON framing over any reader or writer |
| `internal/ansi/ansi.go` | Escape stripping, with an incomplete-tail report (modified) |
| `internal/record/record.go` | The on-disk task record: schema, atomic write, load, scan |
| `internal/daemon/daemon.go` | Listener, accept loop, verb dispatch, shutdown |
| `internal/daemon/registry.go` | Live tasks in memory, name uniqueness, id lookup |
| `internal/daemon/verbs.go` | The six verb handlers |
| `internal/client/client.go` | Connect, implicit daemon start, request and response |
| `internal/paths/paths.go` | The one place that knows where taskd keeps its files |
| `cmd/taskd/main.go` | Subcommand routing for `serve` and the client verbs (modified) |

`internal/paths` exists so that no other package builds a path from `os.UserCacheDir` itself. Plan 1 put `defaultRoot` in `cmd/taskd`; three packages now need the same answer, and a test needs to redirect all of them at once.

## Task Dependency Order

```
Task 1 (proto) ─────┐
Task 2 (ansi)  ─────┼──► Task 4 (daemon core) ──► Task 5 (client) ──┬─► Task 6 (start, status)
Task 3 (record) ────┘                                               ├─► Task 7 (read, search)
                                                                    └─► Task 8 (signal, write)
```

Tasks 6, 7, and 8 each add verbs to a working daemon and are independent of each other.

---

### Task 1: The socket protocol

**Files:**
- Create: `internal/proto/proto.go`, `internal/proto/codec.go`
- Test: `internal/proto/codec_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `proto.Verb`, a string type, with one constant per verb this plan implements
  - `proto.Request{Verb Verb, Params json.RawMessage}`
  - `proto.Response{OK bool, Result json.RawMessage, Error string}`
  - `proto.WriteMessage(w io.Writer, v any) error`
  - `proto.ReadMessage(r io.Reader, v any) error`
  - `proto.MaxMessageBytes`, the cap both directions enforce

The protocol carries one request and one response per connection. Nothing correlates messages, because nothing needs to: the response to a request is the only thing that arrives on that connection. Plan 3's long poll uses the same shape, with a response that arrives late.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	want := Request{Verb: VerbStatus, Params: json.RawMessage(`{"ids":["a"]}`)}
	if err := WriteMessage(&buf, want); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	var got Request
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.Verb != want.Verb {
		t.Fatalf("Verb = %q, want %q", got.Verb, want.Verb)
	}
	if string(got.Params) != string(want.Params) {
		t.Fatalf("Params = %s, want %s", got.Params, want.Params)
	}
}

func TestWriteMessageEndsWithANewline(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, Response{OK: true}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if b := buf.Bytes(); len(b) == 0 || b[len(b)-1] != '\n' {
		t.Fatalf("encoded message does not end with a newline: %q", b)
	}
}

func TestWriteMessageRejectsAnOversizeMessage(t *testing.T) {
	var buf bytes.Buffer
	huge := Response{Error: strings.Repeat("x", MaxMessageBytes+1)}
	if err := WriteMessage(&buf, huge); err == nil {
		t.Fatal("WriteMessage accepted a message over MaxMessageBytes, want an error")
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteMessage wrote %d bytes for a rejected message, want 0", buf.Len())
	}
}

func TestReadMessageRejectsAnOversizeMessage(t *testing.T) {
	line := append(bytes.Repeat([]byte("x"), MaxMessageBytes+1), '\n')
	var got Request
	if err := ReadMessage(bytes.NewReader(line), &got); err == nil {
		t.Fatal("ReadMessage accepted a stream over MaxMessageBytes, want an error")
	}
}

func TestReadMessageRejectsTruncatedJSON(t *testing.T) {
	var got Request
	if err := ReadMessage(strings.NewReader(`{"verb":"task_st`), &got); err == nil {
		t.Fatal("ReadMessage accepted truncated JSON, want an error")
	}
}
```

- [ ] **Step 2: Run the test and watch it fail**

Run: `go test ./internal/proto/`
Expected: FAIL, the package does not build because nothing is defined yet.

- [ ] **Step 3: Write the types**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package proto defines the request and response messages that a taskd
// client and the daemon exchange over a Unix socket.
package proto

import "encoding/json"

// Verb names one operation the daemon performs.
type Verb string

// The verbs this plan implements. Plan 3 adds task_wait.
const (
	VerbStart  Verb = "task_start"
	VerbStatus Verb = "task_status"
	VerbRead   Verb = "task_read"
	VerbSearch Verb = "task_search"
	VerbSignal Verb = "task_signal"
	VerbWrite  Verb = "task_write"
)

// Request is the single message a client sends on a connection.
//
// Params carries the verb's own arguments, left as raw JSON so this package
// does not depend on every verb's parameter type.
type Request struct {
	Verb   Verb            `json:"verb"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the single message the daemon sends back.
//
// OK reports whether the verb succeeded. Exactly one of Result and Error
// carries content: Result on success, Error on failure.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}
```

- [ ] **Step 4: Write the codec**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

import (
	"encoding/json"
	"fmt"
	"io"
)

// MaxMessageBytes caps one encoded message in either direction. A client is
// a local process the user already controls, so this is a guard against a
// runaway encode rather than against an attacker.
const MaxMessageBytes = 1 << 20

// WriteMessage encodes v as JSON followed by a newline.
//
// The newline is not needed to find the message boundary, since a connection
// carries exactly one message in each direction. It is there so an operator
// can read a captured socket stream.
//
// An oversize message is rejected before anything is written, so a failed
// call never leaves a partial message on the connection.
func WriteMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: encode: %w", err)
	}
	if len(b) > MaxMessageBytes {
		return fmt.Errorf("proto: message is %d bytes, over the %d byte limit", len(b), MaxMessageBytes)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("proto: write: %w", err)
	}
	return nil
}

// ReadMessage decodes one JSON message from r into v.
//
// It reads at most MaxMessageBytes, so a peer that never stops writing
// cannot exhaust memory here.
func ReadMessage(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, MaxMessageBytes))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("proto: decode: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/proto/ -v`
Expected: PASS, five tests.

- [ ] **Step 6: Run the gates and commit**

```bash
make check
git add internal/proto/
git commit -m "Add the client and daemon socket protocol"
```

---

### Task 2: An escape stripper that reports an incomplete tail

**Files:**
- Modify: `internal/ansi/ansi.go`
- Test: `internal/ansi/ansi_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `ansi.Strip(b []byte) (clean []byte, pendingLen int)` — a changed signature. Every existing caller must be updated, and `main` holds none today, so this task lands before the readers in Task 7.

This task closes the defect Plan 1 parked. Two things are wrong today.

First, `]` is byte `0x5D`, which falls inside the two-byte escape's final-byte class `[@-Z\\-_]` (`0x5C` through `0x5F`). A complete OSC sequence survives that only because the OSC alternative comes first and Go's alternation is leftmost-first, while a truncated one falls through to the two-byte branch, which consumes `ESC ]` and emits the payload as literal text. The class must not contain `]` at all, so the two regular expressions stay independently correct rather than one masking the other.

Second, and the reason a pure function could not fix it: output arrives in chunks, and a sequence split across a chunk boundary cannot be classified from one chunk. `pendingLen` reports how many trailing bytes begin a sequence this buffer does not finish. A cursor reader rewinds its next cursor by that count so the sequence is re-read whole. A tail or search reader drops them.

- [ ] **Step 1: Write the failing tests**

Add these to the existing `ansi_test.go`, and update the existing tests for the two-value return.

```go
func TestStripReportsAnIncompleteTail(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantClean   string
		wantPending int
	}{
		{"a lone trailing escape", "ok\x1b", "ok", 1},
		{"a truncated CSI", "ok\x1b[31", "ok", 4},
		{"a truncated OSC", "ok\x1b]0;title", "ok", 9},
		{"a truncated OSC on its terminator", "ok\x1b]0;t\x1b", "ok", 6},
		{"a complete OSC is not pending", "ok\x1b]0;t\x07done", "okdone", 0},
		{"a complete CSI is not pending", "ok\x1b[31mdone", "okdone", 0},
		{"no escapes at all", "plain text", "plain text", 0},
		{"an escape mid-buffer is not pending", "a\x1bXb", "ab", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clean, pending := Strip([]byte(c.in))
			if string(clean) != c.wantClean {
				t.Fatalf("clean = %q, want %q", clean, c.wantClean)
			}
			if pending != c.wantPending {
				t.Fatalf("pendingLen = %d, want %d", pending, c.wantPending)
			}
		})
	}
}

func TestStripRejoinsASequenceSplitAcrossChunks(t *testing.T) {
	// The whole stream is "red\x1b[31mtext", cut inside the CSI sequence.
	// A reader that rewinds by pendingLen sees the sequence whole on the
	// next read and never emits its bytes as literal output.
	whole := "red\x1b[31mtext"
	cut := 6 // "red\x1b[3"

	first, pending := Strip([]byte(whole[:cut]))
	if string(first) != "red" {
		t.Fatalf("first chunk clean = %q, want %q", first, "red")
	}

	resume := cut - pending
	second, pendingTwo := Strip([]byte(whole[resume:]))
	if string(second) != "text" {
		t.Fatalf("second chunk clean = %q, want %q", second, "text")
	}
	if pendingTwo != 0 {
		t.Fatalf("second chunk pendingLen = %d, want 0", pendingTwo)
	}
	if got := string(first) + string(second); got != "redtext" {
		t.Fatalf("rejoined = %q, want %q", got, "redtext")
	}
}

func TestStripDoesNotTreatABareCloseBracketAsATwoByteEscape(t *testing.T) {
	// ESC ] starts an OSC sequence. It is never a complete two-byte escape.
	// Before this fix the payload "0;title" was emitted as literal text.
	clean, pending := Strip([]byte("\x1b]0;title"))
	if len(clean) != 0 {
		t.Fatalf("clean = %q, want empty: the whole buffer is an unfinished OSC", clean)
	}
	if pending != 9 {
		t.Fatalf("pendingLen = %d, want 9", pending)
	}
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/ansi/`
Expected: FAIL, the package does not build because `Strip` returns one value.

- [ ] **Step 3: Write the implementation**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ansi removes terminal escape sequences from captured output.
package ansi

import (
	"bytes"
	"regexp"
)

// escape matches, in order: CSI sequences, OSC sequences ended by BEL or by
// the string terminator, and two-byte escape sequences. The pattern operates
// on bytes below 0x80 only, so multi-byte UTF-8 text passes through unchanged.
//
// The two-byte class deliberately excludes both 0x5B and 0x5D. Those bytes
// open a CSI and an OSC sequence, so neither ever ends a two-byte escape.
var escape = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` +
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
		`|\x1b[@-Z\\^_]`,
)

// incomplete matches a sequence that starts in the buffer and is not
// finished by the end of it: a lone escape, a CSI without its final byte, or
// an OSC without its terminator. Every alternative is anchored to the end,
// so a match can only be the buffer's trailing bytes.
//
// Each alternative is prefixed with `\x1b*` to absorb any run of bare ESC
// bytes immediately ahead of the unfinished construct. Without it, FindIndex
// anchors on the last ESC of such a run and strands the earlier ones just
// before the trimmed tail, where escape can never match them (a two-byte
// escape needs a byte after ESC) and they leak into clean unstripped.
// escByte is the raw ESC byte, dropped from clean after escape and the
// pending-tail trim have each had a chance to account for it. See Strip.
var escByte = []byte{0x1b}

var incomplete = regexp.MustCompile(
	`\x1b*\x1b$` +
		`|\x1b*\x1b\[[0-9;?]*[ -/]*$` +
		`|\x1b*\x1b\][^\x07\x1b]*\x1b?$`,
)

// Strip removes terminal escape sequences from b.
//
// clean is a fresh copy of b without the sequences it could classify. It
// never aliases b.
//
// pendingLen counts trailing bytes of b that begin a sequence b does not
// finish. Those bytes are not in clean. A caller reading a stream in chunks
// must rewind its cursor by pendingLen so the sequence arrives whole on the
// next read. A caller holding the whole stream, such as a tail or a search,
// discards them: they are an unfinished sequence, not output.
func Strip(b []byte) (clean []byte, pendingLen int) {
	if loc := incomplete.FindIndex(b); loc != nil {
		pendingLen = len(b) - loc[0]
		b = b[:loc[0]]
	}
	clean = escape.ReplaceAll(b, nil)
	return bytes.ReplaceAll(clean, escByte, nil), pendingLen
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/ansi/ -v`
Expected: PASS, every case including the existing table and the aliasing test.

- [ ] **Step 5: Run the gates and commit**

```bash
make check
git add internal/ansi/
git commit -m "Report an incomplete escape sequence tail when stripping"
```

---

### Task 3: The on-disk task record

**Files:**
- Create: `internal/paths/paths.go`, `internal/record/record.go`
- Test: `internal/paths/paths_test.go`, `internal/record/record_test.go`
- Modify: `cmd/taskd/main.go` (drop `defaultRoot`, call `paths.Root`)

**Interfaces:**
- Consumes: `supervisor.State` from Plan 1.
- Produces:
  - `paths.Root() string`, `paths.SocketPath(root string) string`, `paths.LockPath(root string) string`, `paths.TasksDir(root string) string`
  - `record.Record`, the persisted task
  - `record.Save(dir string, r Record) error`, `record.Load(dir string) (Record, error)`, `record.Scan(root string) ([]Record, error)`

`Record.Exit` is a `*int`, not an `int`. Plan 1 produced three separate defects from a zero-value exit code presented as real, in three different readers. With a pointer, the meaningless case is unrepresentable rather than documented. A task that did not exit normally has no exit code at all.

- [ ] **Step 1: Write the failing test for paths**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootHonorsTheEnvironmentOverride(t *testing.T) {
	t.Setenv(RootEnv, "/tmp/taskd-test-root")
	if got := Root(); got != "/tmp/taskd-test-root" {
		t.Fatalf("Root() = %q, want the override", got)
	}
}

func TestRootFallsBackToTheCacheDirectory(t *testing.T) {
	t.Setenv(RootEnv, "")
	got := Root()
	if got == "" {
		t.Fatal("Root() = empty")
	}
	if !strings.HasSuffix(got, "taskd") {
		t.Fatalf("Root() = %q, want a path ending in taskd", got)
	}
}

func TestDerivedPathsSitUnderTheRoot(t *testing.T) {
	root := "/tmp/r"
	for name, got := range map[string]string{
		"socket": SocketPath(root),
		"lock":   LockPath(root),
		"tasks":  TasksDir(root),
	} {
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Fatalf("%s path %q is not under %q", name, got, root)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/paths/`
Expected: FAIL, the package does not build.

- [ ] **Step 3: Write paths**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package paths is the one place that knows where taskd keeps its files.
//
// The daemon, the client, and the tests all resolve the same root through
// this package, so a test can redirect every one of them with one variable.
package paths

import (
	"os"
	"path/filepath"
)

// RootEnv names the variable that overrides the taskd root directory.
const RootEnv = "TASKD_ROOT"

// Root reports the directory that holds the socket, the lock, and every
// task record.
func Root() string {
	if dir := os.Getenv(RootEnv); dir != "" {
		return dir
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return ".taskd"
	}
	return filepath.Join(cache, "taskd")
}

// SocketPath reports the Unix socket the daemon listens on.
func SocketPath(root string) string { return filepath.Join(root, "taskd.sock") }

// LockPath reports the file a client locks before it starts a daemon.
func LockPath(root string) string { return filepath.Join(root, "daemon.lock") }

// TasksDir reports the directory that holds one subdirectory per task.
func TasksDir(root string) string { return filepath.Join(root, "tasks") }
```

- [ ] **Step 4: Write the failing test for the record**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package record

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/supervisor"
)

func exited(code int) *int { return &code }

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Record{
		ID:      "abc",
		Name:    "build",
		Command: "cargo",
		Args:    []string{"build"},
		Dir:     "/repo",
		PTY:     true,
		State:   supervisor.StateExited,
		Exit:    exited(0),
		Written: 42,
	}
	if err := Save(dir, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ID != want.ID || got.Name != want.Name || got.State != want.State {
		t.Fatalf("Load returned %+v, want the saved record", got)
	}
	if got.Exit == nil || *got.Exit != 0 {
		t.Fatalf("Exit = %v, want a pointer to 0", got.Exit)
	}
}

func TestASignaledRecordCarriesNoExitCode(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{ID: "a", State: supervisor.StateSignaled, Signal: "SIGKILL"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Exit != nil {
		t.Fatalf("Exit = %d, want nil: a signaled task has no exit code", *got.Exit)
	}
	if got.Signal != "SIGKILL" {
		t.Fatalf("Signal = %q, want SIGKILL", got.Signal)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{ID: "a", State: supervisor.StateRunning}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := Save(dir, Record{ID: "a", State: supervisor.StateExited, Exit: exited(3)}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatalf("directory holds %d entries, want only %s: a temp file leaked", len(entries), FileName)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != supervisor.StateExited {
		t.Fatalf("State = %q, want the second write to have replaced the first", got.State)
	}
}

func TestSaveWritesAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Record{ID: "a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

func TestScanReturnsEveryTaskRecord(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"a", "b"} {
		dir := filepath.Join(root, "tasks", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := Save(dir, Record{ID: id, State: supervisor.StateRunning, StartedAt: time.Unix(0, 0)}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}

	got, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Scan returned %d records, want 2", len(got))
	}
}

func TestScanSkipsADirectoryWithNoRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "tasks", "half-made"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d records, want 0: a directory with no record is not a task", len(got))
	}
}

func TestScanOnAMissingTasksDirectoryIsNotAnError(t *testing.T) {
	got, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan on a fresh root: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Scan returned %d records, want 0", len(got))
	}
}
```

- [ ] **Step 5: Run it and watch it fail**

Run: `go test ./internal/record/`
Expected: FAIL, the package does not build.

- [ ] **Step 6: Write the record**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package record stores one task's metadata on disk, next to its log.
package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/supervisor"
)

// FileName is the record's name inside a task directory.
const FileName = "meta.json"

// Record is everything taskd knows about one task, including after the
// process has ended and after the daemon has restarted.
type Record struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	Dir         string   `json:"cwd,omitempty"`
	PTY         bool     `json:"pty"`
	KillAfterS  *int     `json:"kill_after_s"`
	OnOutputCap string   `json:"on_output_cap"`

	Harness string `json:"harness,omitempty"`
	Session string `json:"session,omitempty"`

	State supervisor.State `json:"state"`
	PID   int              `json:"pid,omitempty"`

	// Exit is nil unless State is exited. A task that a signal ended, that
	// never started, or whose status was lost has no exit code, and a zero
	// here would read as success. Keep it a pointer so that case cannot be
	// represented.
	Exit   *int   `json:"exit_code,omitempty"`
	Signal string `json:"signal,omitempty"`

	MaxRSSBytes int64  `json:"max_rss_bytes,omitempty"`
	OutputErr   string `json:"output_err,omitempty"`

	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`

	// Written counts every byte the task ever produced. Retained counts the
	// bytes still on disk. They differ once rotation has discarded output.
	Written  int64 `json:"written"`
	Retained int64 `json:"retained"`
}

// Save writes r into dir atomically.
//
// It writes a temporary file in the same directory and renames it over the
// record, so a reader never sees a half-written record and a crash never
// leaves one.
func Save(dir string, r Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("record: encode %s: %w", r.ID, err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("record: create temp in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }() // no-op once the rename succeeds

	if err := writeAndClose(tmp, b); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("record: rename into %s: %w", dir, err)
	}
	return nil
}

// writeAndClose writes b to f, gives it private permissions, flushes it, and
// closes it. It closes f on every path.
func writeAndClose(f *os.File, b []byte) (err error) {
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("record: close %s: %w", f.Name(), cerr)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return fmt.Errorf("record: chmod %s: %w", f.Name(), err)
	}
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("record: write %s: %w", f.Name(), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("record: sync %s: %w", f.Name(), err)
	}
	return nil
}

// Load reads the record in dir.
func Load(dir string) (Record, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		return Record{}, fmt.Errorf("record: read %s: %w", dir, err)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("record: decode %s: %w", dir, err)
	}
	return r, nil
}

// Scan reads every task record under root.
//
// A task directory with no record is skipped rather than reported: a task
// being created is not a corrupt one. A root with no tasks directory yet is
// not an error either.
func Scan(root string) ([]Record, error) {
	dir := paths.TasksDir(root)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("record: list %s: %w", dir, err)
	}

	var out []Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Load(filepath.Join(dir, e.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
```

- [ ] **Step 7: Point cmd/taskd at paths**

Delete `defaultRoot` from `cmd/taskd/main.go` and replace its one use:

```go
	root := fs.String("root", paths.Root(), "directory that holds task records")
```

Add `"github.com/rcarback/taskd/internal/paths"` to the imports and remove `"os"` only if nothing else in the file uses it. `os.Environ` and `os.Exit` still do, so keep it.

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/paths/ ./internal/record/ ./cmd/taskd/ -v`
Expected: PASS. The `cmd/taskd` suite must still pass unchanged.

- [ ] **Step 9: Run the gates and commit**

```bash
make check
go test ./... -race -count=5
git add internal/paths/ internal/record/ cmd/taskd/
git commit -m "Add task records and one source of taskd paths"
```

---

### Task 4: The daemon core

**Files:**
- Create: `internal/daemon/daemon.go`, `internal/daemon/registry.go`
- Test: `internal/daemon/daemon_test.go`, `internal/daemon/registry_test.go`

**Interfaces:**
- Consumes: `proto`, `record`, `paths`, `supervisor`, `output`, `clock`.
- Produces:
  - `daemon.New(root string, clk clock.Clock) (*Daemon, error)`
  - `(*Daemon).Serve(ctx context.Context) error`, returns nil when ctx ends
  - `(*Daemon).Addr() string`, the socket path it listens on
  - `(*Daemon).Handle(v proto.Verb, h Handler)`, registers one verb
  - `daemon.Handler func(params json.RawMessage) (any, error)`
  - `daemon.Registry`, `daemon.Entry`, and the registry methods below
  - `Entry`'s fields are unexported. Reach them through `Record()`, `SetState()`, `Live()`, `LiveTask()`, `LiveStore()`, `Dir()`, and `Finish()`.

This task delivers a daemon that listens, dispatches, and answers. It registers no verbs. Tasks 6, 7, and 8 add them. A daemon that correctly reports "unknown verb" over a real socket is a testable deliverable, and it keeps the connection handling separate from the verb logic.

The registry holds live tasks. `Entry.Task` is nil once the process has ended, so a finished task still answers `task_status` from its record without the daemon keeping a dead `*supervisor.Task` around.

- [ ] **Step 1: Write the failing registry test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"testing"

	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

func TestRegistryFindsATaskByIDAndByName(t *testing.T) {
	r := NewRegistry()
	e := &Entry{Rec: record.Record{ID: "abc", Name: "build", State: supervisor.StateRunning}}
	if err := r.Add(e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, key := range []string{"abc", "build"} {
		if got, ok := r.Get(key); !ok || got != e {
			t.Fatalf("Get(%q) did not return the entry", key)
		}
	}
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get returned an entry for an unknown key")
	}
}

func TestRegistryRejectsADuplicateLiveName(t *testing.T) {
	r := NewRegistry()
	if err := r.Add(&Entry{Rec: record.Record{ID: "a", Name: "build", State: supervisor.StateRunning}}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	err := r.Add(&Entry{Rec: record.Record{ID: "b", Name: "build", State: supervisor.StateRunning}})
	if err == nil {
		t.Fatal("Add accepted a duplicate name while the first task is running")
	}
}

func TestRegistryFreesANameWhenTheTaskEnds(t *testing.T) {
	r := NewRegistry()
	first := &Entry{Rec: record.Record{ID: "a", Name: "build", State: supervisor.StateRunning}}
	if err := r.Add(first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	first.SetState(supervisor.StateExited)

	if err := r.Add(&Entry{Rec: record.Record{ID: "b", Name: "build", State: supervisor.StateRunning}}); err != nil {
		t.Fatalf("Add after the first task ended: %v", err)
	}
	got, ok := r.Get("build")
	if !ok || got.Rec.ID != "b" {
		t.Fatal("the name did not resolve to the new task")
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("the finished task is no longer reachable by id")
	}
}

func TestRegistryAnAnonymousTaskNeedsNoName(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"a", "b"} {
		if err := r.Add(&Entry{Rec: record.Record{ID: id, State: supervisor.StateRunning}}); err != nil {
			t.Fatalf("Add %s: %v", id, err)
		}
	}
	if n := len(r.List()); n != 2 {
		t.Fatalf("List returned %d entries, want 2", n)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/daemon/`
Expected: FAIL, the package does not build.

- [ ] **Step 3: Write the registry**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"fmt"
	"sort"
	"sync"

	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

// Entry is one task the daemon owns.
//
// Every field is unexported and reached only through the accessors below.
// The mutex is worthless otherwise: a connection goroutine answering a verb
// reads these while the goroutine waiting on the task clears them, and an
// unguarded pointer read concurrent with a pointer write is a data race, not
// merely a stale read.
//
// task and store are nil for a task that has ended: the daemon keeps the
// record so status and reads still answer, and drops the live process and
// its open log file.
type Entry struct {
	mu    sync.Mutex
	rec   record.Record
	task  *supervisor.Task
	store *output.Store
	dir   string
}

// Record returns a copy of the entry's record.
func (e *Entry) Record() record.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rec
}

// SetState updates the entry's state under its own lock.
func (e *Entry) SetState(s supervisor.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.State = s
}

// Live reports whether the task is still running.
func (e *Entry) Live() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rec.State == supervisor.StateRunning
}

// LiveTask returns the running task, or nil once it has ended.
func (e *Entry) LiveTask() *supervisor.Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task
}

// LiveStore returns the open log store, or nil once the task has ended.
func (e *Entry) LiveStore() *output.Store {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.store
}

// Dir returns the task's directory, which never changes after construction.
func (e *Entry) Dir() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dir
}

// Finish records the terminal result in one guarded write and clears both
// live handles together, so no caller can observe a half-finished
// transition.
//
// The switch is total over every state, and Exit is set only under
// StateExited. Three separate Plan 1 defects came from presenting a
// zero-value exit code as a real one; making the state decide which field
// is written keeps that unrepresentable rather than merely documented.
//
// Task 6 replaces this with a version that also takes the byte counts,
// remaps a requested kill to StateKilled, and closes the entry's done
// channel.
func (e *Entry) Finish(res supervisor.Result) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.rec.State = res.State
	switch res.State {
	case supervisor.StateExited:
		code := res.ExitCode
		e.rec.Exit = &code
	case supervisor.StateSignaled:
		e.rec.Signal = res.Signal.String()
	case supervisor.StateRunning, supervisor.StateKilled, supervisor.StateFailed, supervisor.StateLost:
		// Neither field applies: StateRunning is not terminal, and the
		// other three carry no exit code or signal of their own.
	}
	ended := res.Ended
	e.rec.EndedAt = &ended
	if res.OutputErr != nil {
		e.rec.OutputErr = res.OutputErr.Error()
	}
	e.rec.MaxRSSBytes = res.MaxRSSBytes

	e.task = nil
	e.store = nil
}

// Registry holds every task the daemon knows, live or finished.
type Registry struct {
	mu     sync.Mutex
	byID   map[string]*Entry
	byName map[string]*Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[string]*Entry{}, byName: map[string]*Entry{}}
}

// Add registers e.
//
// A name must be unique among live tasks. A name whose previous holder has
// ended is free to reuse, and the finished task stays reachable by its id.
func (r *Registry) Add(e *Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rec := e.Record()
	if _, taken := r.byID[rec.ID]; taken {
		return fmt.Errorf("daemon: task id %s is already registered", rec.ID)
	}
	if rec.Name != "" {
		if prev, taken := r.byName[rec.Name]; taken && prev.Live() {
			return fmt.Errorf("daemon: name %q is in use by a running task", rec.Name)
		}
		r.byName[rec.Name] = e
	}
	r.byID[rec.ID] = e
	return nil
}

// Get finds a task by id, or by the name of the task that currently holds it.
func (r *Registry) Get(key string) (*Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[key]; ok {
		return e, true
	}
	e, ok := r.byName[key]
	return e, ok
}

// List returns every entry, ordered by id so output is stable.
func (r *Registry) List() []*Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*Entry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record().ID < out[j].Record().ID })
	return out
}
```

- [ ] **Step 4: Write the failing daemon test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

// start runs a daemon on a temporary root and returns it with its socket
// path. It stops the daemon when the test ends.
func start(t *testing.T) (*Daemon, string) {
	t.Helper()
	root := t.TempDir()
	d, err := New(root, clock.System())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v, want nil after cancellation", err)
		}
	})
	return d, paths.SocketPath(root)
}

// roundTrip sends one request and returns the response.
func roundTrip(t *testing.T, sock string, req proto.Request) proto.Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := proto.WriteMessage(conn, req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var res proto.Response
	if err := proto.ReadMessage(conn, &res); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return res
}

func TestDaemonAnswersARegisteredVerb(t *testing.T) {
	d, sock := start(t)
	d.Handle(proto.VerbStatus, func(json.RawMessage) (any, error) {
		return map[string]string{"hello": "world"}, nil
	})

	res := roundTrip(t, sock, proto.Request{Verb: proto.VerbStatus})
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
	if string(res.Result) != `{"hello":"world"}` {
		t.Fatalf("Result = %s, want the handler's value", res.Result)
	}
}

func TestDaemonRejectsAnUnknownVerb(t *testing.T) {
	_, sock := start(t)

	res := roundTrip(t, sock, proto.Request{Verb: "task_nonsense"})
	if res.OK {
		t.Fatal("OK = true for an unknown verb")
	}
	if res.Error == "" {
		t.Fatal("Error is empty for an unknown verb")
	}
}

func TestDaemonReportsAHandlerError(t *testing.T) {
	d, sock := start(t)
	d.Handle(proto.VerbStatus, func(json.RawMessage) (any, error) {
		return nil, errBoom
	})

	res := roundTrip(t, sock, proto.Request{Verb: proto.VerbStatus})
	if res.OK {
		t.Fatal("OK = true when the handler failed")
	}
	if res.Error != errBoom.Error() {
		t.Fatalf("Error = %q, want %q", res.Error, errBoom.Error())
	}
}

func TestDaemonSurvivesAMalformedRequest(t *testing.T) {
	_, sock := start(t)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = conn.Close()

	// The same daemon must still answer the next client on the same socket.
	res := roundTrip(t, sock, proto.Request{Verb: "task_nonsense"})
	if res.OK {
		t.Fatal("OK = true for an unknown verb")
	}
	if res.Error == "" {
		t.Fatal("the daemon answered with no error text after a malformed request")
	}
}

func TestDaemonSocketIsPrivate(t *testing.T) {
	_, sock := start(t)

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestNewMarksAnOrphanedRunningTaskLost(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(paths.TasksDir(root), "orphan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := record.Save(dir, record.Record{
		ID:        "orphan",
		State:     supervisor.StateRunning,
		StartedAt: time.Unix(0, 0),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := New(root, clock.System()); err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := record.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != supervisor.StateLost {
		t.Fatalf("State = %q, want %q: the daemon that owned it is gone", got.State, supervisor.StateLost)
	}
}

func TestNewLeavesAFinishedRecordAlone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(paths.TasksDir(root), "done")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	code := 3
	if err := record.Save(dir, record.Record{ID: "done", State: supervisor.StateExited, Exit: &code}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := New(root, clock.System()); err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := record.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != supervisor.StateExited || got.Exit == nil || *got.Exit != 3 {
		t.Fatalf("record changed to %+v, want the finished record untouched", got)
	}
}
```

Add to the test file:

```go
var errBoom = errors.New("boom")
```

with `"errors"` imported.

- [ ] **Step 5: Run it and watch it fail**

Run: `go test ./internal/daemon/`
Expected: FAIL, `New`, `Serve`, and `Handle` are undefined.

- [ ] **Step 6: Write the daemon**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemon runs the taskd supervisor process: it owns every task and
// answers client requests on a Unix socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

// Handler answers one verb. It returns the value to encode into
// Response.Result, or an error to report in Response.Error.
type Handler func(params json.RawMessage) (any, error)

// Daemon owns every task on this machine for this user.
type Daemon struct {
	Root string
	Clk  clock.Clock
	Reg  *Registry

	ln net.Listener

	mu       sync.RWMutex
	handlers map[proto.Verb]Handler
}

// New prepares the taskd root, reconciles records left by a previous daemon,
// and listens on the socket.
//
// New listens rather than Serve, so a caller knows the socket exists as soon
// as New returns and a test never races the accept loop.
func New(root string, clk clock.Clock) (*Daemon, error) {
	if err := os.MkdirAll(paths.TasksDir(root), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", root, err)
	}
	if err := reconcile(root, clk); err != nil {
		return nil, err
	}

	sock := paths.SocketPath(root)
	if err := clearStaleSocket(sock); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: chmod %s: %w", sock, err)
	}

	return &Daemon{
		Root:     root,
		Clk:      clk,
		Reg:      NewRegistry(),
		ln:       ln,
		handlers: map[proto.Verb]Handler{},
	}, nil
}

// reconcile marks every task that a previous daemon left running as lost.
//
// When the daemon dies its children die with it, and their exit codes die
// with them. Reporting lost is the honest answer and keeps a restart
// correct without re-adopting orphaned processes.
func reconcile(root string, clk clock.Clock) error {
	records, err := record.Scan(root)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.State != supervisor.StateRunning {
			continue
		}
		r.State = supervisor.StateLost
		ended := clk.Now()
		r.EndedAt = &ended
		if err := record.Save(taskDir(root, r.ID), r); err != nil {
			return err
		}
	}
	return nil
}

// clearStaleSocket removes a socket file that no daemon is listening on.
//
// A live daemon answers a dial, so a successful dial means this process must
// not take the address. Anything else means the file is left over from a
// daemon that died, and Listen would fail on it.
func clearStaleSocket(sock string) error {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon: another daemon is already listening on %s", sock)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale socket %s: %w", sock, err)
	}
	return nil
}

// taskDir reports the directory holding one task's record and log.
func taskDir(root, id string) string {
	return filepath.Join(paths.TasksDir(root), id)
}

// Addr reports the socket path the daemon listens on.
func (d *Daemon) Addr() string { return d.ln.Addr().String() }

// Handle registers h for v, replacing any previous handler.
func (d *Daemon) Handle(v proto.Verb, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[v] = h
}

// Serve accepts connections until ctx ends, then returns nil.
func (d *Daemon) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = d.ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := d.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daemon: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.serveConn(conn)
		}()
	}
}

// serveConn reads one request, answers it, and closes the connection.
//
// A failure to read the request is the client's problem and cannot be
// reported to it, so the connection simply closes. A failure in the handler
// is reported in the response.
func (d *Daemon) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var req proto.Request
	if err := proto.ReadMessage(conn, &req); err != nil {
		return
	}
	_ = proto.WriteMessage(conn, d.answer(req))
}

// answer runs the handler for req and builds the response.
func (d *Daemon) answer(req proto.Request) proto.Response {
	d.mu.RLock()
	h, ok := d.handlers[req.Verb]
	d.mu.RUnlock()

	if !ok {
		return proto.Response{OK: false, Error: fmt.Sprintf("daemon: unknown verb %q", req.Verb)}
	}
	result, err := h(req.Params)
	if err != nil {
		return proto.Response{OK: false, Error: err.Error()}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return proto.Response{OK: false, Error: fmt.Sprintf("daemon: encode result: %v", err)}
	}
	return proto.Response{OK: true, Result: b}
}
```

Add `"path/filepath"` to the imports for `taskDir`.

- [ ] **Step 7: Run the tests**

Run: `go test ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 8: Run the gates and commit**

```bash
make check
go test ./... -race -count=5
git add internal/daemon/
git commit -m "Add the daemon listener, dispatch, and task registry"
```

---

### Task 5: The client and implicit daemon start

**Files:**
- Create: `internal/client/client.go`
- Test: `internal/client/client_test.go`
- Modify: `cmd/taskd/main.go` (add the `serve` subcommand)

**Interfaces:**
- Consumes: `proto`, `paths`, `daemon`, `clock`.
- Produces:
  - `client.Call(root string, req proto.Request) (proto.Response, error)`
  - `client.Dial(root string) (net.Conn, error)`, connects and starts a daemon if needed

This is the task the spec calls out as the concurrency risk: eight clients against a cold socket must start exactly one daemon.

The sequence is:

1. Try to connect. If that works, there is nothing to start.
2. Take an exclusive `flock`.
3. Try to connect again. Another client may have finished starting a daemon while this one waited for the lock.
4. If that still fails, spawn the daemon and wait for the socket.
5. Release the lock.

Step 3 is what makes this correct. Without it, every client that queued on the lock spawns a daemon of its own.

Go cannot call `fork` safely from a multithreaded runtime, so the daemon is a re-exec of this same binary. The spec calls for `setsid`, and macOS ships no such binary, so use `syscall.SysProcAttr{Setsid: true}`, which Darwin and Linux both support and which needs no external program.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
)

// buildTaskd compiles the binary once per test run so the client has a real
// program to re-exec.
func buildTaskd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "taskd")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/rcarback/taskd/cmd/taskd")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestCallReachesARunningDaemon(t *testing.T) {
	root := t.TempDir()
	d, err := daemon.New(root, clock.System())
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	d.Handle(proto.VerbStatus, func(json.RawMessage) (any, error) {
		return map[string]int{"n": 1}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	res, err := Call(root, proto.Request{Verb: proto.VerbStatus})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
}

func TestCallStartsADaemonWhenNoneIsListening(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.RootEnv, root)
	t.Setenv(BinaryEnv, buildTaskd(t))

	res, err := Call(root, proto.Request{Verb: proto.VerbStatus})
	if err != nil {
		t.Fatalf("Call on a cold root: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
	if _, err := os.Stat(paths.SocketPath(root)); err != nil {
		t.Fatalf("no socket after Call: %v", err)
	}
	t.Cleanup(func() { stopDaemon(t, root) })
}

func TestConcurrentClientsStartExactlyOneDaemon(t *testing.T) {
	root := t.TempDir()
	t.Setenv(paths.RootEnv, root)
	t.Setenv(BinaryEnv, buildTaskd(t))
	t.Cleanup(func() { stopDaemon(t, root) })

	const clients = 8
	var wg sync.WaitGroup
	errs := make([]error, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Call(root, proto.Request{Verb: proto.VerbStatus})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
	if n := countDaemons(t, root); n != 1 {
		t.Fatalf("%d daemons running, want exactly 1", n)
	}
}

// countDaemons counts taskd serve processes started against this test's
// root. The root is a fresh temporary directory, so the match cannot catch
// a daemon belonging to another test or to the developer's own machine.
func countDaemons(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "taskd serve --root "+root).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return 0 // pgrep reports "no match" with status 1
		}
		t.Skipf("pgrep unavailable, cannot count daemons: %v", err)
	}
	return len(strings.Fields(string(out)))
}

// stopDaemon ends the daemon this test started.
func stopDaemon(t *testing.T, root string) {
	t.Helper()
	_ = exec.Command("pkill", "-f", "taskd serve --root "+root).Run()
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/client/`
Expected: FAIL, the package does not build.

- [ ] **Step 3: Write the client**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package client connects to the taskd daemon, starting one if none is
// listening.
package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
)

// BinaryEnv names the variable that overrides which binary the client
// re-execs as the daemon. Tests set it. In normal use the client re-execs
// itself.
const BinaryEnv = "TASKD_BINARY"

// startTimeout bounds how long a client waits for a daemon it started to
// begin listening.
const startTimeout = 10 * time.Second

// pollInterval is how often the client retries the socket while it waits.
const pollInterval = 10 * time.Millisecond

// Call sends one request and returns the daemon's response, starting a
// daemon first if none is listening.
func Call(root string, req proto.Request) (proto.Response, error) {
	conn, err := Dial(root)
	if err != nil {
		return proto.Response{}, err
	}
	defer func() { _ = conn.Close() }()

	if err := proto.WriteMessage(conn, req); err != nil {
		return proto.Response{}, err
	}
	var res proto.Response
	if err := proto.ReadMessage(conn, &res); err != nil {
		return proto.Response{}, err
	}
	return res, nil
}

// Dial connects to the daemon, starting one if the socket is missing or
// stale.
func Dial(root string) (net.Conn, error) {
	sock := paths.SocketPath(root)
	if conn, err := net.Dial("unix", sock); err == nil {
		return conn, nil
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("client: create %s: %w", root, err)
	}

	unlock, err := lockDaemonStart(root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Another client may have started a daemon while this one waited for
	// the lock. Without this second attempt, every queued client starts its
	// own daemon in turn.
	if conn, err := net.Dial("unix", sock); err == nil {
		return conn, nil
	}

	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client: remove stale socket %s: %w", sock, err)
	}
	if err := spawnDaemon(root); err != nil {
		return nil, err
	}
	return waitForSocket(sock)
}

// lockDaemonStart takes an exclusive lock so that concurrent clients start
// exactly one daemon. The returned function releases it.
func lockDaemonStart(root string) (func(), error) {
	path := paths.LockPath(root)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("client: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("client: lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// spawnDaemon re-execs this binary as the daemon in its own session.
//
// Go cannot call fork safely from a multithreaded runtime, so re-exec is the
// mechanism rather than a workaround. Setsid detaches the daemon from the
// client's terminal and process group, so it survives the shell that started
// the client. The spec says "under setsid"; macOS ships no setsid binary, so
// this uses the syscall both platforms support.
func spawnDaemon(root string) error {
	bin := os.Getenv(BinaryEnv)
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("client: find this binary: %w", err)
		}
		bin = self
	}

	cmd := exec.Command(bin, "serve", "--root", root) //nolint:gosec // bin is this binary, or a test's override
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("client: start daemon: %w", err)
	}
	// The daemon outlives this client, so release the child handle rather
	// than waiting for it. It is in its own session and init reaps it.
	return cmd.Process.Release()
}

// waitForSocket dials the socket until the daemon answers or startTimeout
// elapses.
func waitForSocket(sock string) (net.Conn, error) {
	deadline := time.Now().Add(startTimeout)
	for {
		if conn, err := net.Dial("unix", sock); err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("client: daemon did not listen on %s within %s", sock, startTimeout)
		}
		time.Sleep(pollInterval)
	}
}
```

`waitForSocket` is the one place in the repository that sleeps. It is production code waiting on a real process, not a test waiting on a time-based condition, so `forbidigo` must allow it. Add the exemption to `.golangci.yml` scoped to this file rather than removing the rule:

```yaml
  exclusions:
    rules:
      - path: internal/client/client.go
        linters:
          - forbidigo
```

The test file imports `"errors"`, `"os/exec"`, and `"strings"` for those helpers.

- [ ] **Step 4: Add the serve subcommand**

In `cmd/taskd/main.go`, extend `dispatch`:

```go
func dispatch(args []string, stdout io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stdout, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return run(args[1:], stdout)
	case "serve":
		return serve(args[1:], stdout)
	default:
		fmt.Fprintln(stdout, usage)
		return 2
	}
}

const usage = "usage: taskd run [flags] -- COMMAND [ARGS...]\n       taskd serve [--root DIR]"

// serve runs the daemon in the foreground until a signal ends it.
func serve(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", paths.Root(), "directory that holds task records")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	d, err := daemon.New(*root, clock.System())
	if err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := d.Serve(ctx); err != nil {
		fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	return 0
}
```

Keep the existing `_, _ =` form on every `fmt.Fprint*` call, which `errcheck` requires.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/client/ ./cmd/taskd/ -race -v`
Expected: PASS. Run the concurrency test repeatedly: `go test ./internal/client/ -race -run Concurrent -count=10`.

- [ ] **Step 6: Run the gates and commit**

```bash
make check
go test ./... -race -count=5
git add internal/client/ cmd/taskd/ .golangci.yml
git commit -m "Start the daemon implicitly from the client"
```

---

### Task 6: The task_start and task_status verbs

**Files:**
- Create: `internal/daemon/verbs.go`, `internal/daemon/params.go`
- Test: `internal/daemon/verbs_test.go`
- Modify: `cmd/taskd/main.go` (register the handlers in `serve`), `internal/output/store.go` (add `Counts`)

**Interfaces:**
- Consumes: `Registry`, `Entry`, `Handler` from Task 4; `record`, `supervisor`, `output`, `taskdir`.
- Produces:
  - `daemon.StartParams`, `daemon.StartResult`
  - `daemon.StatusParams`, `daemon.StatusEntry`, `daemon.StatusResult`
  - `(*Daemon).Register()`, which installs every handler this plan implements

`Register` exists so `cmd/taskd` calls one method rather than listing handlers. Tasks 7 and 8 extend it.

Three behaviors need care:

**A cap that kills produces `killed`, not `signaled`.** The spec reserves `killed` for a cap the caller opted into, or for `task_signal`. A cap sends SIGKILL, so the supervisor reports `signaled`. The entry records that taskd asked for the kill, and the waiter maps the result to `killed`. Task 8 reuses the same flag.

**`kill_after_s` defaults to null and nothing else creates a cap.** A task with no cap runs until it ends on its own or a client signals it.

**`on_output_cap: "kill"` is rejected, not ignored.** Plan 2 implements `rotate`. The value `kill` needs an overflow signal that `output.Store` does not have, and adding one belongs with Plan 3's pattern work. The handler validates the field and returns an error that names Plan 3, so a caller learns what happened and nothing is silently dropped.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/supervisor"
)

// newDaemon returns a registered daemon on a temporary root, without a
// listener, so a verb test calls handlers directly.
func newDaemon(t *testing.T) *Daemon {
	t.Helper()
	d, err := New(t.TempDir(), clock.System())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.Register()
	return d
}

// callVerb runs one handler with params encoded from v.
func callVerb(t *testing.T, d *Daemon, verb string, v any) (any, error) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	d.mu.RLock()
	h := d.handlers[proto.Verb(verb)]
	d.mu.RUnlock()
	if h == nil {
		t.Fatalf("no handler for %s", verb)
	}
	return h(b)
}

// waitForState blocks until the entry reaches a terminal state.
func waitForState(t *testing.T, e *Entry) supervisor.State {
	t.Helper()
	select {
	case <-e.Done():
		return e.Record().State
	case <-time.After(10 * time.Second):
		t.Fatal("task did not reach a terminal state")
		return ""
	}
}

func TestStartRunsATaskAndRecordsItsExit(t *testing.T) {
	d := newDaemon(t)

	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "echo hi; exit 7"}, Name: "job",
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	res, ok := got.(StartResult)
	if !ok {
		t.Fatalf("result type %T, want StartResult", got)
	}

	e, ok := d.Reg.Get(res.ID)
	if !ok {
		t.Fatal("the started task is not in the registry")
	}
	if state := waitForState(t, e); state != supervisor.StateExited {
		t.Fatalf("State = %q, want exited", state)
	}
	rec := e.Record()
	if rec.Exit == nil || *rec.Exit != 7 {
		t.Fatalf("Exit = %v, want 7", rec.Exit)
	}
	if rec.Written == 0 {
		t.Fatal("Written = 0, want the bytes the task produced")
	}
}

func TestStartRejectsADuplicateLiveName(t *testing.T) {
	d := newDaemon(t)
	p := StartParams{Command: "sh", Args: []string{"-c", "sleep 30"}, Name: "job"}

	if _, err := callVerb(t, d, "task_start", p); err != nil {
		t.Fatalf("first task_start: %v", err)
	}
	if _, err := callVerb(t, d, "task_start", p); err == nil {
		t.Fatal("task_start accepted a duplicate name while the first task runs")
	}
}

func TestStartRejectsAnEmptyCommand(t *testing.T) {
	d := newDaemon(t)
	if _, err := callVerb(t, d, "task_start", StartParams{}); err == nil {
		t.Fatal("task_start accepted an empty command")
	}
}

func TestStartRejectsTheKillOutputCap(t *testing.T) {
	d := newDaemon(t)
	_, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, OnOutputCap: "kill",
	})
	if err == nil {
		t.Fatal("task_start accepted on_output_cap=kill, which Plan 2 does not implement")
	}
}

func TestStartDefaultsToAPseudoTerminal(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "test -t 1 && echo yes || echo no"},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	e, _ := d.Reg.Get(got.(StartResult).ID)
	waitForState(t, e)
	if !e.Record().PTY {
		t.Fatal("PTY = false, want true by default")
	}
}

func TestAnOptedInCapReportsKilledNotSignaled(t *testing.T) {
	d := newDaemon(t)
	one := 1
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "sleep 60"}, KillAfterS: &one,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	e, _ := d.Reg.Get(got.(StartResult).ID)

	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: taskd asked for this kill", state)
	}
	if e.Record().Exit != nil {
		t.Fatal("Exit is set for a killed task, want nil")
	}
}

func TestStatusWithNoIDsListsEveryTask(t *testing.T) {
	d := newDaemon(t)
	for range 2 {
		if _, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "exit 0"}}); err != nil {
			t.Fatalf("task_start: %v", err)
		}
	}

	got, err := callVerb(t, d, "task_status", StatusParams{})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	if n := len(got.(StatusResult).Tasks); n != 2 {
		t.Fatalf("listed %d tasks, want 2", n)
	}
}

func TestStatusByIDReportsOnlyThatTask(t *testing.T) {
	d := newDaemon(t)
	first, _ := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "exit 0"}})
	if _, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "exit 0"}}); err != nil {
		t.Fatalf("second task_start: %v", err)
	}

	id := first.(StartResult).ID
	got, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{id}})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	tasks := got.(StatusResult).Tasks
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("task_status returned %+v, want only %s", tasks, id)
	}
}

func TestStatusReportsAnUnknownIDAsAnError(t *testing.T) {
	d := newDaemon(t)

	_, err := callVerb(t, d, "task_status", StatusParams{IDs: []string{"no-such-task"}})
	if err == nil {
		t.Fatal("task_status succeeded for an id nobody owns")
	}
	if !strings.Contains(err.Error(), "no-such-task") {
		t.Fatalf("error = %q, want it to name the missing id", err)
	}
}

func TestStatusFiltersBySession(t *testing.T) {
	d := newDaemon(t)
	if _, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, Session: "s1",
	}); err != nil {
		t.Fatalf("task_start: %v", err)
	}
	if _, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "exit 0"}, Session: "s2",
	}); err != nil {
		t.Fatalf("task_start: %v", err)
	}

	got, err := callVerb(t, d, "task_status", StatusParams{Session: "s1"})
	if err != nil {
		t.Fatalf("task_status: %v", err)
	}
	tasks := got.(StatusResult).Tasks
	if len(tasks) != 1 || tasks[0].Session != "s1" {
		t.Fatalf("session filter returned %+v, want only the s1 task", tasks)
	}
}
```

The test file imports `"strings"` and `"github.com/rcarback/taskd/internal/proto"`.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/daemon/`
Expected: FAIL, `Register`, `StartParams`, and `Entry.Done` are undefined.

- [ ] **Step 3: Write the parameter types**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import "time"

// StartParams is task_start's input.
//
// PTY is a pointer so an omitted field means "use the default", which is
// true, rather than false.
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
```

- [ ] **Step 4: Extend Entry with completion and kill tracking**

Add to `registry.go`:

```go
// Task 4 already defines Entry with unexported fields and the guarded
// accessors Record, SetState, Live, LiveTask, LiveStore, Dir, and Finish.
// This task ADDS two unexported fields and the methods below. Do not
// redeclare the struct and do not re-add an accessor Task 4 already has.
//
// done closes when the task reaches a terminal state. Callers use Done.
// killRequested records that taskd asked for the kill, so the waiter can
// report killed rather than signaled.
type Entry struct {
	mu    sync.Mutex
	rec   record.Record
	task  *supervisor.Task
	store *output.Store
	dir   string

	// added by this task
	done          chan struct{}
	killRequested bool
}

// NewEntry returns an entry whose Done channel is ready to use.
func NewEntry(rec record.Record, dir string) *Entry {
	return &Entry{rec: rec, dir: dir, done: make(chan struct{})}
}

// Done closes once the task has reached a terminal state.
func (e *Entry) Done() <-chan struct{} { return e.done }

// AttachStore and AttachTask record the live handles. They are separate
// because the store exists before the process does: the entry joins the
// registry with its store so a status read can find it, and the task is
// set only once supervisor.Start has succeeded. Both take the same lock
// Finish uses to clear the handles.
func (e *Entry) AttachStore(store *output.Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.store = store
}

func (e *Entry) AttachTask(task *supervisor.Task) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.task = task
}

// handles returns the task and its store together under one lock, for the
// single goroutine that waits on the task and then closes its store.
//
// Reading the fields one at a time through LiveTask and LiveStore would let
// Finish run between the two reads and hand back a mismatched pair.
func (e *Entry) handles() (*supervisor.Task, *output.Store) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.task, e.store
}

// RequestKill records that taskd asked for this task to end, so its
// terminal state is killed rather than signaled.
func (e *Entry) RequestKill() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.killRequested = true
}

// Finish records the terminal result and closes Done exactly once.
//
// This task REPLACES the Finish that Task 4 defined, which took only a
// Result. The extra parameters carry the byte counts, which the caller
// reads from the store before this call clears the handle, and the return
// value is the record to persist. The kill remap and the Done close are
// also added here.
func (e *Entry) Finish(res supervisor.Result, written, retained int64) record.Record {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.rec.State = res.State
	if res.State == supervisor.StateSignaled {
		e.rec.Signal = res.Signal.String()
		if e.killRequested {
			// The spec reserves killed for a cap taskd applied or a client
			// signal. The process really did die on a signal; what makes
			// this killed rather than signaled is that taskd asked.
			e.rec.State = supervisor.StateKilled
		}
	}
	if res.State == supervisor.StateExited {
		code := res.ExitCode
		e.rec.Exit = &code
	}
	if res.OutputErr != nil {
		e.rec.OutputErr = res.OutputErr.Error()
	}
	e.rec.MaxRSSBytes = res.MaxRSSBytes
	e.rec.Written = written
	e.rec.Retained = retained
	ended := res.Ended
	e.rec.EndedAt = &ended

	// Clear both live handles under this lock. Entry.Log reads the store
	// under the same lock and must never hand a caller a store that await
	// has already closed.
	e.task = nil
	e.store = nil
	close(e.done)
	return e.rec
}
```

`Finish` sets `Exit` only under `StateExited`, which is the constraint three Plan 1 defects violated. Keep that guard, and keep the state switch total over every state so a new state cannot silently fall through.

Everywhere else in this task and in Tasks 7 and 8, reach an entry's data through the accessors rather than the fields: `e.Dir()` not `e.Dir`, `e.Record()` not `e.Rec`, `e.LiveTask()` and `e.LiveStore()` not `e.Task` and `e.Store`, and `e.AttachStore(store)` / `e.AttachTask(task)` to set the live handles. The fields are unexported precisely so a connection goroutine cannot read one while the waiter clears it.

- [ ] **Step 5: Write the handlers**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rcarback/taskd/internal/output"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
	"github.com/rcarback/taskd/internal/taskdir"
)

// Register installs every verb handler this plan implements.
func (d *Daemon) Register() {
	d.Handle(proto.VerbStart, jsonHandler(d.start))
	d.Handle(proto.VerbStatus, jsonHandler(d.status))
}

// jsonHandler adapts a typed handler into a Handler by decoding its params.
func jsonHandler[P any, R any](fn func(P) (R, error)) Handler {
	return func(raw json.RawMessage) (any, error) {
		var p P
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("daemon: decode params: %w", err)
			}
		}
		return fn(p)
	}
}

// start spawns a task and returns its id.
func (d *Daemon) start(p StartParams) (StartResult, error) {
	if p.Command == "" {
		return StartResult{}, fmt.Errorf("daemon: task_start needs a command")
	}
	outputCap := p.OnOutputCap
	if outputCap == "" {
		outputCap = defaultOnOutputCap
	}
	if outputCap != defaultOnOutputCap {
		return StartResult{}, fmt.Errorf(
			"daemon: on_output_cap %q is not implemented yet; only %q is. A killing cap needs the overflow signal that arrives with pattern support",
			outputCap, defaultOnOutputCap)
	}

	dir, id, err := taskdir.New(d.Root)
	if err != nil {
		return StartResult{}, err
	}

	maxOutput := p.MaxOutput
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutput
	}
	store, err := output.Open(filepath.Join(dir, "out.log"), maxOutput)
	if err != nil {
		return StartResult{}, err
	}

	usePTY := p.PTY == nil || *p.PTY
	rec := record.Record{
		ID: id, Name: p.Name,
		Command: p.Command, Args: p.Args, Dir: p.Cwd,
		PTY: usePTY, KillAfterS: p.KillAfterS, OnOutputCap: outputCap,
		Harness: p.Harness, Session: p.Session,
		State: supervisor.StateRunning, StartedAt: d.Clk.Now(),
	}

	e := NewEntry(rec, dir)
	e.AttachStore(store)
	if err := d.Reg.Add(e); err != nil {
		_ = store.Close()
		return StartResult{}, err
	}

	task, err := supervisor.Start(supervisor.Spec{
		Command: p.Command, Args: p.Args, Dir: p.Cwd,
		Env: os.Environ(), PTY: usePTY,
	}, storeWriter{store}, d.Clk)
	if err != nil {
		_ = store.Close()
		e.SetState(supervisor.StateFailed)
		_ = record.Save(dir, e.Record())
		return StartResult{}, err
	}
	e.AttachTask(task)

	if err := record.Save(dir, rec); err != nil {
		return StartResult{}, err
	}

	go d.await(e)
	if p.KillAfterS != nil {
		go d.enforceCap(e, *p.KillAfterS)
	}
	return StartResult{ID: id, Name: p.Name}, nil
}

// await waits for the task to end, then records the outcome.
func (d *Daemon) await(e *Entry) {
	task, store := e.handles()
	res := task.Wait()
	written, retained := store.Counts()
	_ = store.Close()

	rec := e.Finish(res, written, retained)
	_ = record.Save(e.Dir(), rec)
}

// enforceCap ends the task after seconds, if it is still running.
//
// This is the only path in Plan 2 that ends a task without a client asking,
// and it runs only when the caller set kill_after_s. Nothing creates a cap
// by default.
func (d *Daemon) enforceCap(e *Entry, seconds int) {
	select {
	case <-e.Done():
	case <-d.Clk.After(time.Duration(seconds) * time.Second):
		task := e.LiveTask()
		if task == nil {
			return // it ended while the timer was firing
		}
		e.RequestKill()
		_ = task.Signal(syscall.SIGKILL)
	}
}

// status reports terse state for the requested tasks, or lists.
func (d *Daemon) status(p StatusParams) (StatusResult, error) {
	if len(p.IDs) > 0 {
		out := make([]StatusEntry, 0, len(p.IDs))
		for _, id := range p.IDs {
			e, ok := d.Reg.Get(id)
			if !ok {
				return StatusResult{}, fmt.Errorf("daemon: no task %q", id)
			}
			out = append(out, statusOf(e))
		}
		return StatusResult{Tasks: out}, nil
	}

	var out []StatusEntry
	for _, e := range d.Reg.List() {
		rec := e.Record()
		if !p.All && p.Session != "" && rec.Session != p.Session {
			continue
		}
		out = append(out, statusOf(e))
	}
	return StatusResult{Tasks: out}, nil
}

// statusOf converts an entry to its terse form.
func statusOf(e *Entry) StatusEntry {
	r := e.Record()
	return StatusEntry{
		ID: r.ID, Name: r.Name, State: string(r.State), Command: r.Command,
		Exit: r.Exit, Signal: r.Signal, Session: r.Session,
		Written: r.Written, Retained: r.Retained,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, OutputErr: r.OutputErr,
	}
}

// storeWriter adapts an output.Store to io.Writer.
//
// Exactly one storeWriter value is built per task and passed once, so
// os/exec collapses stdout and stderr onto one copy goroutine.
type storeWriter struct{ s *output.Store }

func (w storeWriter) Write(p []byte) (int, error) {
	if err := w.s.Append(p); err != nil {
		return 0, err
	}
	return len(p), nil
}
```

Add `"syscall"` and `"time"` to the imports. The local is named `outputCap` rather than `cap` because `cap` shadows a builtin and `predeclared` flags it.

- [ ] **Step 6: Add Store.Counts**

`await` needs both byte counts, and reading them through two separate calls
lets a running task change one between them. Add a single accessor to
`internal/output/store.go`:

```go
// Counts reports the total bytes the task ever produced and the bytes still
// held on disk. They are read together under one lock, so a caller can
// subtract them and never see two different moments.
func (s *Store) Counts() (written, retained int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written, s.written - s.base
}
```

Test that `retained` equals `written` before any rotation, that `retained` is
smaller after rotation has discarded output, and that `written` still counts
every byte the task produced.

- [ ] **Step 7: Register the handlers in serve**

In `cmd/taskd/main.go`, call `d.Register()` after `daemon.New`.

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/daemon/ ./internal/output/ -race -v`
Expected: PASS.

- [ ] **Step 9: Run the gates and commit**

```bash
make check
go test ./... -race -count=5
git add internal/daemon/ internal/output/ cmd/taskd/
git commit -m "Add task_start and task_status"
```

---

### Task 7: The task_read and task_search verbs

**Files:**
- Modify: `internal/output/store.go` (add `OpenExisting`), `internal/daemon/verbs.go`, `internal/daemon/params.go`, `internal/daemon/registry.go`
- Test: `internal/output/store_test.go`, `internal/daemon/read_test.go`

**Interfaces:**
- Consumes: `ansi.Strip` from Task 2, `Registry` and `Entry` from Tasks 4 and 6.
- Produces:
  - `output.OpenExisting(path string, written, retained int64) (*Store, error)`, a read-only store over a finished task's log
  - `(*Entry).Log() (*Store, func(), error)`, the live store or a read-only one
  - `daemon.ReadParams`, `daemon.ReadResult`, `daemon.SearchParams`, `daemon.SearchResult`

A finished task's store is closed, so a read after completion reopens the log read-only. The record already holds `Written` and `Retained`, which is exactly what `OpenExisting` needs to restore the absolute-offset invariant: `base = written - retained`.

**The cursor and the incomplete tail.** A cursor read strips escape sequences from the bytes it returns and rewinds `next` by `ansi.Strip`'s `pendingLen`, so a sequence cut by the read boundary arrives whole next time. Two cases need explicit handling:

- If every byte read is pending and the task is still running, return no data and leave `next` at the caller's cursor. The caller retries when more output exists. Returning a rewound cursor that makes no progress would spin.
- If every byte read is pending and the task has ended, those bytes are a sequence the task never finished. Drop them and set `next` to the end. Holding them forever would make `eof` unreachable.

- [ ] **Step 1: Write the failing tests**

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"
)

func startAndWait(t *testing.T, d *Daemon, script string) string {
	t.Helper()
	got, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)
	waitForState(t, e)
	return id
}

func TestReadReturnsTheLogFromACursor(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'alpha\\nbeta\\n'")

	got, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	res := got.(ReadResult)
	if !strings.Contains(res.Data, "alpha") || !strings.Contains(res.Data, "beta") {
		t.Fatalf("Data = %q, want both lines", res.Data)
	}
	if res.Next == 0 {
		t.Fatal("Next = 0 after reading output")
	}
	if !res.EOF {
		t.Fatal("EOF = false for a finished task read to the end")
	}
}

func TestReadFromACursorReturnsEachByteOnce(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'alpha\\nbeta\\n'")

	first, err := callVerb(t, d, "task_read", ReadParams{ID: id, MaxBytes: 4})
	if err != nil {
		t.Fatalf("first task_read: %v", err)
	}
	a := first.(ReadResult)

	second, err := callVerb(t, d, "task_read", ReadParams{ID: id, Since: &a.Next})
	if err != nil {
		t.Fatalf("second task_read: %v", err)
	}
	b := second.(ReadResult)

	if joined := a.Data + b.Data; !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") {
		t.Fatalf("the two reads joined to %q, want the whole log exactly once", joined)
	}
}

func TestReadStripsEscapeSequences(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf '\\033[31mred\\033[0m\\n'")

	got, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	res := got.(ReadResult)
	if strings.Contains(res.Data, "\x1b") {
		t.Fatalf("Data = %q, want no escape bytes", res.Data)
	}
	if !strings.Contains(res.Data, "red") {
		t.Fatalf("Data = %q, want the text", res.Data)
	}
}

func TestReadTailReturnsTheLastLines(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "for i in 1 2 3 4 5; do echo line$i; done")

	two := 2
	got, err := callVerb(t, d, "task_read", ReadParams{ID: id, Tail: &two})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	res := got.(ReadResult)
	if strings.Contains(res.Data, "line3") {
		t.Fatalf("Data = %q, want only the last two lines", res.Data)
	}
	if !strings.Contains(res.Data, "line5") {
		t.Fatalf("Data = %q, want the last line", res.Data)
	}
}

func TestReadReportsTruncatedBytesAfterRotation(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "for i in $(seq 1 500); do echo padding-line-$i; done"},
		MaxOutput: 1024,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)
	waitForState(t, e)

	res, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if res.(ReadResult).TruncatedBytes == 0 {
		t.Fatal("TruncatedBytes = 0 after rotation discarded output")
	}
}

func TestReadRejectsBothSinceAndTail(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "echo hi")

	zero, two := int64(0), 2
	if _, err := callVerb(t, d, "task_read", ReadParams{ID: id, Since: &zero, Tail: &two}); err == nil {
		t.Fatal("task_read accepted both since and tail")
	}
}

func TestSearchFindsMatchesWithContext(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'one\\ntwo\\nerror: boom\\nfour\\nfive\\n'")

	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "error:", Context: 1})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 1 {
		t.Fatalf("found %d matches, want 1", len(res.Matches))
	}
	m := res.Matches[0]
	if !strings.Contains(m.Line, "boom") {
		t.Fatalf("Line = %q, want the matching line", m.Line)
	}
	if len(m.Before) != 1 || !strings.Contains(m.Before[0], "two") {
		t.Fatalf("Before = %v, want the preceding line", m.Before)
	}
	if len(m.After) != 1 || !strings.Contains(m.After[0], "four") {
		t.Fatalf("After = %v, want the following line", m.After)
	}
}

func TestSearchRejectsABadRegex(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "echo hi")

	if _, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "("}); err == nil {
		t.Fatal("task_search accepted an invalid regular expression")
	}
}

func TestSearchCapsTheMatchCount(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "for i in 1 2 3 4 5; do echo error; done")

	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "error", MaxMatches: 2})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 2 {
		t.Fatalf("found %d matches, want the cap of 2", len(res.Matches))
	}
	if !res.More {
		t.Fatal("More = false, want true when the cap hid matches")
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/daemon/`
Expected: FAIL, `ReadParams` and the read handler are undefined.

- [ ] **Step 3: Add the read-only store**

```go
// OpenExisting opens a finished task's log for reading only.
//
// written and retained come from the task's record. The file holds exactly
// the last retained bytes of the stream, so the first byte on disk is at
// stream offset written-retained and every cursor keeps its meaning.
func OpenExisting(path string, written, retained int64) (*Store, error) {
	if retained < 0 || written < retained {
		return nil, fmt.Errorf("output: written=%d retained=%d is not a valid log", written, retained)
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0) //nolint:gosec // the caller's own task log
	if err != nil {
		return nil, fmt.Errorf("output: open %s: %w", path, err)
	}
	return &Store{
		path: path, file: f, maxBytes: retained + 1,
		written: written, base: written - retained, readOnly: true,
	}, nil
}
```

Add `readOnly bool` to `Store` and guard `Append`:

```go
	if s.readOnly {
		return fmt.Errorf("output: %s is open for reading only", s.path)
	}
```

- [ ] **Step 4: Add Entry.Log**

```go
// Log returns a store for reading this task's output, plus a release
// function the caller must call.
//
// A running task uses the live store, which the daemon keeps open, and the
// release is a no-op. A finished task's store is closed, so this opens the
// log read-only from the record's byte counts and the release closes it.
func (e *Entry) Log() (*output.Store, func(), error) {
	e.mu.Lock()
	live, rec, dir := e.LiveStore(), e.Record(), e.Dir()
	e.mu.Unlock()

	if live != nil {
		return live, func() {}, nil
	}
	s, err := output.OpenExisting(filepath.Join(dir, "out.log"), rec.Written, rec.Retained)
	if err != nil {
		return nil, nil, err
	}
	return s, func() { _ = s.Close() }, nil
}
```

`finish` already clears `Store` under the entry's lock, and `await` closes it just before calling `finish`. `Log` reads `Store` under the same lock, so it never sees a closed store.

- [ ] **Step 5: Write the parameters and handlers**

```go
// ReadParams is task_read's input. Since and Tail are mutually exclusive.
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
type Match struct {
	LineNumber int      `json:"line_number"`
	Line       string   `json:"line"`
	Before     []string `json:"before,omitempty"`
	After      []string `json:"after,omitempty"`
}

// SearchResult is task_search's output. More reports that MaxMatches hid
// further matches.
type SearchResult struct {
	Matches        []Match `json:"matches"`
	TruncatedBytes int64   `json:"truncated_bytes"`
	More           bool    `json:"more"`
}

const (
	defaultReadBytes  = 64 << 10
	defaultMaxMatches = 50
)
```

```go
// read returns output by cursor, or the last N lines.
func (d *Daemon) read(p ReadParams) (ReadResult, error) {
	if p.Since != nil && p.Tail != nil {
		return ReadResult{}, fmt.Errorf("daemon: task_read takes since or tail, not both")
	}
	e, ok := d.Reg.Get(p.ID)
	if !ok {
		return ReadResult{}, fmt.Errorf("daemon: no task %q", p.ID)
	}
	store, release, err := e.Log()
	if err != nil {
		return ReadResult{}, err
	}
	defer release()

	if p.Tail != nil {
		raw, err := store.Tail(*p.Tail)
		if err != nil {
			return ReadResult{}, err
		}
		clean, _ := ansi.Strip(raw)
		written, retained := store.Counts()
		return ReadResult{
			Data: string(clean), Next: written,
			TruncatedBytes: written - retained, EOF: !e.Live(),
		}, nil
	}

	cursor := int64(0)
	if p.Since != nil {
		cursor = *p.Since
	}
	limit := p.MaxBytes
	if limit <= 0 {
		limit = defaultReadBytes
	}

	raw, next, truncated, err := store.ReadSince(cursor, limit)
	if err != nil {
		return ReadResult{}, err
	}
	clean, pending := ansi.Strip(raw)
	next = rewind(next, cursor, pending, e.Live())

	return ReadResult{
		Data: string(clean), Next: next, TruncatedBytes: truncated,
		EOF: !e.Live() && next >= store.Written(),
	}, nil
}

// rewind pulls next back so an escape sequence cut by the read boundary is
// re-read whole on the next call.
//
// A rewind that makes no progress would spin, so a running task simply
// returns the caller's own cursor and waits for more output. A finished task
// has no more output, so the unfinished sequence is dropped rather than held
// forever, which would make EOF unreachable.
func rewind(next, cursor int64, pending int, live bool) int64 {
	if pending == 0 {
		return next
	}
	rewound := next - int64(pending)
	if rewound > cursor {
		return rewound
	}
	if live {
		return cursor
	}
	return next
}

// search runs a regular expression over the retained log.
func (d *Daemon) search(p SearchParams) (SearchResult, error) {
	re, err := regexp.Compile(p.Regex)
	if err != nil {
		return SearchResult{}, fmt.Errorf("daemon: task_search regex: %w", err)
	}
	e, ok := d.Reg.Get(p.ID)
	if !ok {
		return SearchResult{}, fmt.Errorf("daemon: no task %q", p.ID)
	}
	store, release, err := e.Log()
	if err != nil {
		return SearchResult{}, err
	}
	defer release()

	// ReadSince clamps a cursor below base up to base and reports what
	// rotation discarded, so starting at 0 reads the whole retained log
	// without reading Written and Retained as two separate, racing counters.
	_, retained := store.Counts()
	raw, _, truncated, err := store.ReadSince(0, int(retained)+1)
	if err != nil {
		return SearchResult{}, err
	}
	clean, _ := ansi.Strip(raw)
	lines := strings.Split(strings.TrimSuffix(string(clean), "\n"), "\n")

	maxMatches := p.MaxMatches
	if maxMatches <= 0 {
		maxMatches = defaultMaxMatches
	}

	var out []Match
	more := false
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		if len(out) == maxMatches {
			more = true
			break
		}
		out = append(out, Match{
			LineNumber: i + 1, Line: line,
			Before: window(lines, i-p.Context, i),
			After:  window(lines, i+1, i+1+p.Context),
		})
	}
	return SearchResult{Matches: out, TruncatedBytes: truncated, More: more}, nil
}

// window returns lines[lo:hi], clamped to the slice.
func window(lines []string, lo, hi int) []string {
	if lo < 0 {
		lo = 0
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	if lo >= hi {
		return nil
	}
	return append([]string(nil), lines[lo:hi]...)
}
```

Add to `Register`:

```go
	d.Handle(proto.VerbRead, jsonHandler(d.read))
	d.Handle(proto.VerbSearch, jsonHandler(d.search))
```

Import `"regexp"`, `"strings"`, and the `ansi` package.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/daemon/ ./internal/output/ -race -v`
Expected: PASS.

- [ ] **Step 7: Run the gates and commit**

```bash
make check
go test ./... -race -count=5
git add internal/daemon/ internal/output/
git commit -m "Add task_read and task_search"
```

---

### Task 8: The task_signal and task_write verbs

**Files:**
- Modify: `internal/supervisor/runner.go`, `internal/supervisor/pty.go`, `internal/daemon/verbs.go`, `internal/daemon/params.go`
- Test: `internal/supervisor/runner_test.go`, `internal/daemon/control_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces:
  - `supervisor.Spec.Stdin bool`, opting a task into an input channel
  - `(*supervisor.Task).Write(p []byte) (int, error)`, `(*supervisor.Task).CloseInput() error`
  - `daemon.SignalParams`, `daemon.SignalResult`, `daemon.WriteParams`, `daemon.WriteResult`

`task_signal` sends SIGTERM, then SIGKILL after a grace period, and the task's terminal state is `killed` rather than `signaled` because taskd asked. `Entry.RequestKill` from Task 6 already carries that.

A task on a pseudo-terminal always has an input channel: the master. A task on a pipe needs one requested before it starts, because `os/exec` fixes stdin at `Start`. `Spec.Stdin` opts in.

- [ ] **Step 1: Write the failing supervisor test**

```go
func TestWriteSendsInputToAPipeTask(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "cat", Stdin: true}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := task.Write([]byte("ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := task.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}

	if got := task.Wait(); got.State != StateExited {
		t.Fatalf("State = %q, want exited", got.State)
	}
	if !strings.Contains(out.String(), "ping") {
		t.Fatalf("output = %q, want the input echoed back", out.String())
	}
}

func TestWriteWithoutAnInputChannelFails(t *testing.T) {
	task, err := Start(Spec{Command: "sh", Args: []string{"-c", "exit 0"}}, io.Discard, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	task.Wait()

	if _, err := task.Write([]byte("x")); err == nil {
		t.Fatal("Write succeeded on a task with no input channel")
	}
}

func TestWriteSendsInputToAPTYTask(t *testing.T) {
	var out bytes.Buffer
	task, err := Start(Spec{Command: "cat", PTY: true}, &out, clock.System())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := task.Write([]byte("ping\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// A terminal ends input on EOT rather than on a closed descriptor.
	if _, err := task.Write([]byte{4}); err != nil {
		t.Fatalf("Write EOT: %v", err)
	}
	task.Wait()
	if !strings.Contains(out.String(), "ping") {
		t.Fatalf("output = %q, want the input echoed back", out.String())
	}
}
```

- [ ] **Step 2: Run and watch them fail**

Run: `go test ./internal/supervisor/`
Expected: FAIL, `Spec.Stdin`, `Task.Write`, and `Task.CloseInput` are undefined.

- [ ] **Step 3: Add input to the supervisor**

In `Spec`, add:

```go
	// Stdin opts a task on a plain pipe into an input channel. A task on a
	// pseudo-terminal always has one, because the master is writable.
	// os/exec fixes stdin at Start, so this cannot be added later.
	Stdin bool
```

In `Task`, add `stdin io.WriteCloser`. In `Start`, on the pipe path before `cmd.Start`:

```go
	if spec.Stdin {
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("supervisor: open stdin for %s: %w", spec.Command, err)
		}
		stdin = in
	}
```

and assign it into the `Task`. Then:

```go
// Write sends p to the task's input.
//
// A task on a pseudo-terminal writes to the master, which the child sees as
// terminal input. A task on a pipe writes to the pipe, which it must have
// requested with Spec.Stdin.
func (t *Task) Write(p []byte) (int, error) {
	if t.pty != nil {
		n, err := t.pty.master.Write(p)
		if err != nil {
			return n, fmt.Errorf("supervisor: write to terminal: %w", err)
		}
		return n, nil
	}
	if t.stdin == nil {
		return 0, fmt.Errorf("supervisor: task has no input channel; start it with Spec.Stdin or a pseudo-terminal")
	}
	n, err := t.stdin.Write(p)
	if err != nil {
		return n, fmt.Errorf("supervisor: write to stdin: %w", err)
	}
	return n, nil
}

// CloseInput signals end of input on a pipe task.
//
// A pseudo-terminal task has no equivalent: closing the master would end the
// output copy as well. Send EOT (byte 4) instead, which the line discipline
// turns into end of input.
func (t *Task) CloseInput() error {
	if t.stdin == nil {
		return fmt.Errorf("supervisor: task has no input channel to close")
	}
	if err := t.stdin.Close(); err != nil {
		return fmt.Errorf("supervisor: close stdin: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Write the failing daemon test**

```go
func TestSignalEndsATaskAndReportsKilled(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", "sleep 60"}})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID

	if _, err := callVerb(t, d, "task_signal", SignalParams{ID: id}); err != nil {
		t.Fatalf("task_signal: %v", err)
	}

	e, _ := d.Reg.Get(id)
	if state := waitForState(t, e); state != supervisor.StateKilled {
		t.Fatalf("State = %q, want killed: a client asked for this", state)
	}
	if e.Record().Exit != nil {
		t.Fatal("Exit is set for a killed task, want nil")
	}
}

func TestSignalOnAFinishedTaskFails(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "exit 0")

	if _, err := callVerb(t, d, "task_signal", SignalParams{ID: id}); err == nil {
		t.Fatal("task_signal succeeded on a task that already ended")
	}
}

func TestWriteSendsInputToARunningTask(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{Command: "cat"})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID

	if _, err := callVerb(t, d, "task_write", WriteParams{ID: id, Data: "hello\n"}); err != nil {
		t.Fatalf("task_write: %v", err)
	}
	if _, err := callVerb(t, d, "task_write", WriteParams{ID: id, Data: "\x04"}); err != nil {
		t.Fatalf("task_write EOT: %v", err)
	}

	e, _ := d.Reg.Get(id)
	waitForState(t, e)

	res, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if !strings.Contains(res.(ReadResult).Data, "hello") {
		t.Fatalf("Data = %q, want the input echoed back", res.(ReadResult).Data)
	}
}
```

- [ ] **Step 5: Write the handlers**

```go
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

// defaultGraceSeconds is how long a task has to exit on SIGTERM before
// taskd sends SIGKILL.
const defaultGraceSeconds = 10
```

```go
// signal asks a task to stop, then insists after a grace period.
func (d *Daemon) signal(p SignalParams) (SignalResult, error) {
	e, ok := d.Reg.Get(p.ID)
	if !ok {
		return SignalResult{}, fmt.Errorf("daemon: no task %q", p.ID)
	}
	task := e.LiveTask()
	if task == nil {
		return SignalResult{}, fmt.Errorf("daemon: task %q has already ended", p.ID)
	}

	sig := syscall.SIGTERM
	if strings.EqualFold(p.Signal, "KILL") {
		sig = syscall.SIGKILL
	}

	// The task ends because taskd asked, so its terminal state is killed
	// rather than signaled. Record that before the signal lands, or the
	// waiter can finish first and report signaled.
	e.RequestKill()
	if err := task.Signal(sig); err != nil {
		return SignalResult{}, err
	}

	if sig == syscall.SIGTERM {
		grace := defaultGraceSeconds
		if p.GraceS != nil {
			grace = *p.GraceS
		}
		go d.insist(e, task, grace)
	}
	return SignalResult{ID: p.ID, Signal: sig.String()}, nil
}

// insist sends SIGKILL if the task has not ended within grace seconds.
func (d *Daemon) insist(e *Entry, task *supervisor.Task, grace int) {
	select {
	case <-e.Done():
	case <-d.Clk.After(time.Duration(grace) * time.Second):
		_ = task.Signal(syscall.SIGKILL)
	}
}

// write sends input to a running task.
func (d *Daemon) write(p WriteParams) (WriteResult, error) {
	e, ok := d.Reg.Get(p.ID)
	if !ok {
		return WriteResult{}, fmt.Errorf("daemon: no task %q", p.ID)
	}
	task := e.LiveTask()
	if task == nil {
		return WriteResult{}, fmt.Errorf("daemon: task %q has already ended", p.ID)
	}
	n, err := task.Write([]byte(p.Data))
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{Written: n}, nil
}
```

`Entry.LiveTask` already exists from Task 6. Add to `Register`:

```go
	d.Handle(proto.VerbSignal, jsonHandler(d.signal))
	d.Handle(proto.VerbWrite, jsonHandler(d.write))
```

`start` must pass `Stdin: !usePTY` in the `supervisor.Spec` so a pipe task can still receive input.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/supervisor/ ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 7: Manual verification of the whole plan**

```bash
make build
export TASKD_ROOT=$(mktemp -d)

# A task starts, and the client starts the daemon on the way.
./bin/taskd serve --root "$TASKD_ROOT" &
sleep 1

# Confirm the six verbs answer over the socket, using any small client
# script or `nc -U "$TASKD_ROOT/taskd.sock"` with one JSON line.
```

Expected: `task_start` returns an id, `task_status` lists it, `task_read` returns its output with no escape bytes, `task_search` finds a line, `task_signal` ends a sleeping task and `task_status` then reports `killed`, and `task_write` feeds `cat`.

- [ ] **Step 8: Run the gates and commit**

```bash
make check
go test ./... -race -count=5
git add internal/supervisor/ internal/daemon/
git commit -m "Add task_signal and task_write"
```

---

## What this plan does not build

Named here so a reviewer does not read them as gaps:

- `task_wait`, every wake condition, pattern matching, and notification or long-poll delivery. Plan 3.
- The `index` file, and reading a task's log across a daemon restart. Plan 3.
- `on_output_cap: "kill"`. Rejected with an error that says so, until Plan 3 adds the overflow signal `output.Store` needs.
- The MCP server and the Pi extension. Plan 4.
- The skill, the installer, and the anti-sleep guard. Plan 5.
