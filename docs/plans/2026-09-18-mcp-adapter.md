# MCP Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose the seven taskd verbs as Model Context Protocol tools over standard input and output, so an agent under Claude Code or Codex can start, watch, and read a task without a poll loop.

**Architecture:** A new package at `adapters/mcp` wraps `internal/client`. Each tool marshals a typed input into a `proto.Request`, sends it with `client.Call`, and unmarshals the daemon's result into a typed output. The package exposes no logic of its own except in `task_wait`, which branches on the harness. `cmd/taskd` gains an `mcp` subcommand that runs the server on the standard streams.

**Tech Stack:** Go 1.27, `github.com/modelcontextprotocol/go-sdk` v1.8.0, the existing `internal/client` and `internal/proto` packages.

**Spec:** `docs/design/2026-09-17-taskd-design.md`

## Global Constraints

Every task's requirements include this section.

- **SPDX first.** Every new Go file starts with `// SPDX-License-Identifier: AGPL-3.0-or-later` as its first line. The `goheader` linter fails the build otherwise.
- **No `time.Sleep`.** The `forbidigo` linter forbids it. Use `internal/clock` or a channel.
- **No attribution.** Never write `Co-Authored-By`, `Generated with Claude Code`, or any reference to Claude, Anthropic, or AI tooling into a commit message, a code comment, a document, or any other output.
- **A nil slice marshals to JSON `null`, not `[]`.** Allocate every slice that crosses the wire, even when empty. This defect has recurred in every plan in this project.
- **Socket paths are 104 bytes on macOS.** A test that starts a daemon must use a short root. Never pass `t.TempDir()` as a daemon root: it embeds the test name and overflows `sun_path`.
- **No `t.Parallel()` in a test that starts a daemon.** The daemon package carries a package-level test seam.
- **Tool names are exactly these seven**, from the spec's Tool surface table: `task_start`, `task_wait`, `task_status`, `task_read`, `task_search`, `task_signal`, `task_write`. No others, and no renames.
- **Wake conditions never kill.** Nothing in this adapter sends a signal except `task_signal`.
- **Prose:** no contractions. Run `vale <file>` on every markdown file you change and fix the findings.
- **Enabled linters:** `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`, `bodyclose`, `errorlint`, `gosec`, `misspell`, `revive`, `goheader`, `forbidigo`. Check every returned error. Wrap errors with `%w`.

## Design decisions

Three decisions are settled. Do not revisit them during implementation.

**1. Mirror types, not the daemon's parameter structs.** `internal/daemon` defines `StartParams`, `WaitParams`, and the rest. Those carry fields an agent must never see: `Harness`, `Session`, and `MaxOutput` on `StartParams`. This adapter defines its own input and output types that match the spec's Tool surface exactly, and translates them. A translation test guards against drift.

**2. The harness is a flag, not a detection.** The spec records that Codex gives an MCP server nine environment variables, and none of them names the session, the thread, or the working directory. A server cannot identify its harness from its environment. `taskd mcp --harness <name>` takes the value from the configuration file that launched it. The default is `generic`.

**3. Codex delivery is out of scope.** The spec's Open items 1 and 2 record that `codex queue` waking an idle session is unverified, and that Codex credits were exhausted before it could be measured. This plan ships no Codex delivery path. Under `--harness codex`, `task_wait` behaves exactly as it does under `generic`: it blocks. Add the delivery path when the behavior can be measured.

## What changes for the agent

Today `task_wait` always blocks, on every harness. After this plan, one case stops blocking: `deliver: "notify"` under `--harness claude-code` returns an instruction naming a shell command, and the agent runs that command in the background. The harness then notifies the agent when it exits. Every other combination still blocks and still carries the long-poll warning.

## File Structure

| File | Responsibility |
|------|----------------|
| `adapters/mcp/server.go` | Harness type, server construction, `Run` on the standard streams |
| `adapters/mcp/call.go` | The one generic helper every tool uses to reach the daemon |
| `adapters/mcp/tools.go` | The five pass-through tools and their types |
| `adapters/mcp/start.go` | `task_start`, its pattern and cap types, and the translation to `daemon.StartParams` |
| `adapters/mcp/wait.go` | `task_wait` and the harness branch |
| `cmd/taskd/main.go` | The `mcp` subcommand |

Tests sit beside each file. `adapters/mcp/harness_test.go` holds the shared test helper that starts a daemon on a short root and connects an in-memory client.

---

### Task 1: Dependency, server skeleton, and the `mcp` subcommand

Ends with a server that lists one working tool over an in-memory transport, and a `taskd mcp` subcommand that runs it on the standard streams. This proves the whole chain before any other tool is written.

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `adapters/mcp/server.go`
- Create: `adapters/mcp/call.go`
- Create: `adapters/mcp/harness_test.go`
- Create: `adapters/mcp/server_test.go`
- Modify: `cmd/taskd/main.go:41-57`
- Modify: `cmd/taskd/main_test.go`

**Interfaces:**
- Consumes: `client.Call(root string, req proto.Request) (proto.Response, error)`; `proto.Request{Verb, Params}`; `proto.Response{OK, Result, Error}`; `paths.Root() string`; `version.String() string`; `daemon.StatusParams`, `daemon.StatusResult`, `daemon.StatusEntry`.
- Produces: `mcpadapter.Harness`, a string type, with `ParseHarness(string) (Harness, error)`, `mcpadapter.New(root string, h Harness) *mcp.Server`, `mcpadapter.Run(ctx context.Context, root string, h Harness) error`, and the unexported generic `call[In, Out any]`.

**Naming:** the package is `package mcpadapter` in directory `adapters/mcp`. The SDK's own package is `mcp`, and a local package of the same name would force an import alias in every file.

- [ ] **Step 1: Add the dependency**

```bash
cd /Users/carback1/Code/taskd
go get github.com/modelcontextprotocol/go-sdk@v1.8.0
go mod tidy
```

Expected: `go.mod` gains `github.com/modelcontextprotocol/go-sdk v1.8.0` in the first `require` block, beside `github.com/creack/pty`.

- [ ] **Step 2: Write the failing server test**

Create `adapters/mcp/server_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerRegistersItsTools(t *testing.T) {
	cs := newSession(t)

	got := map[string]bool{}
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatalf("listing tools: %v", err)
		}
		got[tool.Name] = true
	}

	// Task 1 registers task_status alone. Later tasks add the rest and
	// extend this list.
	for _, name := range []string{"task_status"} {
		if !got[name] {
			t.Errorf("tool %q is not registered; got %v", name, got)
		}
	}
}

func TestStatusWithNoIDsListsNothingOnAFreshRoot(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_status",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("calling task_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_status reported an error: %v", res.Content)
	}
}
```

- [ ] **Step 3: Write the test helper**

Create `adapters/mcp/harness_test.go`. A macOS `sun_path` holds 104 bytes, and `t.TempDir()` embeds the test name, so the root has to be short.

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpadapter "github.com/rcarback/taskd/adapters/mcp"
)

// shortRoot returns a daemon root short enough for a Unix socket path.
//
// A macOS sun_path holds 104 bytes and t.TempDir() embeds the test name,
// which overflows it for any test with a descriptive name.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatalf("creating a root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Clean(dir)
}

// newSession connects an in-memory client to a server on a fresh root.
//
// No t.Parallel in any test that calls this: the call starts a daemon.
func newSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := mcpadapter.New(shortRoot(t), mcpadapter.HarnessGeneric)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// decodeStructured re-encodes a tool result's structured content and decodes
// it into T.
//
// CallToolResult.StructuredContent is an any holding whatever the handler
// returned, so a direct type assertion couples every test to the SDK's
// internal representation of that value. A JSON round trip does not.
func decodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-encoding structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding structured content: %v", err)
	}
	return out
}
```

The helper file imports `encoding/json` for this.

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test ./adapters/mcp/ -run TestServerRegisters -v`
Expected: FAIL to build, with `undefined: mcpadapter.New`.

- [ ] **Step 5: Write the call helper**

Create `adapters/mcp/call.go`. Every tool goes through this one function, so error handling is written once.

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"encoding/json"
	"fmt"

	"github.com/rcarback/taskd/internal/client"
	"github.com/rcarback/taskd/internal/proto"
)

// call sends one verb to the daemon and decodes its result.
//
// It starts a daemon if none is listening, because client.Call does. A
// daemon-reported failure becomes an ordinary error, which AddTool turns
// into a tool result with IsError set, so the agent reads the message
// rather than a transport failure.
func call[In, Out any](root string, verb proto.Verb, in In) (Out, error) {
	var out Out

	params, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("mcp: encode %s params: %w", verb, err)
	}

	res, err := client.Call(root, proto.Request{Verb: verb, Params: params})
	if err != nil {
		return out, fmt.Errorf("mcp: %s: %w", verb, err)
	}
	if !res.OK {
		return out, fmt.Errorf("mcp: %s: %s", verb, res.Error)
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return out, fmt.Errorf("mcp: decode %s result: %w", verb, err)
	}
	return out, nil
}
```

- [ ] **Step 6: Write the server**

Create `adapters/mcp/server.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcpadapter exposes the taskd verbs as Model Context Protocol
// tools over standard input and output.
//
// The package name differs from its directory because the SDK's own package
// is named mcp, and two packages with one name would force an alias in
// every file that imports both.
package mcpadapter

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/version"
)

// Harness names the agent runtime that launched this server.
//
// A server cannot work this out for itself. Codex passes an MCP server nine
// environment variables and none of them names the session, the thread, or
// the working directory, so the value arrives as a flag from the
// configuration that launched the server.
type Harness string

// The harnesses this adapter recognizes.
const (
	// HarnessGeneric blocks on every wait. It is the default.
	HarnessGeneric Harness = "generic"

	// HarnessClaudeCode returns a background-command instruction for a
	// wait that asks for notify delivery.
	HarnessClaudeCode Harness = "claude-code"

	// HarnessCodex behaves as HarnessGeneric. Codex delivery is unverified
	// and this adapter ships none.
	HarnessCodex Harness = "codex"
)

// ParseHarness converts a flag value into a Harness.
func ParseHarness(s string) (Harness, error) {
	switch h := Harness(s); h {
	case HarnessGeneric, HarnessClaudeCode, HarnessCodex:
		return h, nil
	default:
		return "", fmt.Errorf(
			"mcp: unknown harness %q: want generic, claude-code, or codex", s)
	}
}

// server holds what every tool handler needs.
type server struct {
	root    string
	harness Harness
}

// New builds an MCP server that serves the taskd tools from one root.
func New(root string, h Harness) *mcp.Server {
	s := &server{root: root, harness: h}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "taskd", Version: version.String()}, nil)
	s.addStatus(srv)
	return srv
}

// Run serves the tools on the standard streams until the client
// disconnects.
func Run(ctx context.Context, root string, h Harness) error {
	if err := New(root, h).Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}

// StatusInput is task_status's input. With no ids it lists every task.
type StatusInput struct {
	IDs []string `json:"ids,omitempty" jsonschema:"task ids to report; omit to list every task"`
	All bool     `json:"all,omitempty"  jsonschema:"include tasks that have already ended"`
}

// addStatus registers task_status.
func (s *server) addStatus(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_status",
		Description: "Terse state for one or more tasks. With no ids, list " +
			"every task. Read the state before you report a task as done: " +
			"a fired condition is not a successful one.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in StatusInput,
	) (*mcp.CallToolResult, daemon.StatusResult, error) {
		out, err := call[daemon.StatusParams, daemon.StatusResult](
			s.root, proto.VerbStatus, daemon.StatusParams{
				IDs:     in.IDs,
				Session: "",
				All:     in.All,
			})
		return nil, out, err
	})
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./adapters/mcp/ -v`
Expected: PASS, both tests.

If `daemon.StatusResult` marshals `Tasks` as `null` on an empty root, that is the nil-slice constraint biting. Do not patch it here: check whether `statusOf` already allocates, and report it if it does not.

- [ ] **Step 8: Wire the subcommand**

In `cmd/taskd/main.go`, add the case to `dispatch` and extend `usage`:

```go
	case "mcp":
		return runMCP(args[1:], stdout)
```

```go
const usage = "usage: taskd run [flags] -- COMMAND [ARGS...]\n" +
	"       taskd wait --id ID [--id ID...] [--until exit,idle:300]\n" +
	"       taskd mcp [--root DIR] [--harness NAME]\n" +
	"       taskd serve [--root DIR]"
```

Add the handler beside `serve`:

```go
// runMCP serves the tools over standard input and output.
//
// Nothing else may write to stdout while this runs: the transport is
// newline-delimited JSON on that stream, and a stray line corrupts it. Errors
// go to the caller's stdout only after Run returns.
func runMCP(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("taskd mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	root := fs.String("root", paths.Root(), "directory that holds task records")
	harness := fs.String("harness", string(mcpadapter.HarnessGeneric),
		"agent runtime: generic, claude-code, or codex")
	if err := fs.Parse(args); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	h, err := mcpadapter.ParseHarness(*harness)
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := mcpadapter.Run(ctx, *root, h); err != nil {
		_, _ = fmt.Fprintf(stdout, "taskd: %v\n", err)
		return 1
	}
	return 0
}
```

Add the import: `mcpadapter "github.com/rcarback/taskd/adapters/mcp"`.

- [ ] **Step 9: Test the subcommand's argument handling**

Add to `cmd/taskd/main_test.go`:

```go
func TestMCPRejectsAnUnknownHarness(t *testing.T) {
	var out strings.Builder
	if got := dispatch([]string{"mcp", "--harness", "emacs"}, &out); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(out.String(), "unknown harness") {
		t.Errorf("output = %q, want it to name the unknown harness", out.String())
	}
}

func TestUsageNamesTheMCPSubcommand(t *testing.T) {
	var out strings.Builder
	if got := dispatch(nil, &out); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(out.String(), "taskd mcp") {
		t.Errorf("usage = %q, want it to name the mcp subcommand", out.String())
	}
}
```

- [ ] **Step 10: Run the full gate**

Run: `go build ./... && go test ./... -race && make check`
Expected: build clean, tests pass, `make check` reports 0 issues.

- [ ] **Step 11: Commit**

```bash
git add go.mod go.sum adapters/mcp cmd/taskd
git commit -m "mcp: add the adapter skeleton and the taskd mcp subcommand"
```

---

### Task 2: The four remaining pass-through tools

`task_read`, `task_search`, `task_signal`, and `task_write` have no logic. Each marshals an input, calls a verb, and returns the result. They are one batch because they are the same shape four times.

**Files:**
- Create: `adapters/mcp/tools.go`
- Create: `adapters/mcp/tools_test.go`
- Modify: `adapters/mcp/server.go` (register them in `New`)
- Modify: `adapters/mcp/server_test.go` (extend the expected tool list)

**Interfaces:**
- Consumes: `call`, `server`, the daemon's `ReadParams`, `ReadResult`, `SearchParams`, `SearchResult`, `SignalParams`, `SignalResult`, `WriteParams`, `WriteResult`.
- Produces: `ReadInput`, `SearchInput`, `SignalInput`, `WriteInput`, and four `add*` methods on `server`.

- [ ] **Step 1: Write the failing test**

Add to `adapters/mcp/tools_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReadRejectsAnUnknownTaskWithAReadableMessage(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_read",
		Arguments: map[string]any{"id": "no-such-task"},
	})
	if err != nil {
		t.Fatalf("calling task_read: %v", err)
	}
	if !res.IsError {
		t.Fatal("task_read accepted an unknown id")
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "no-such-task") {
		t.Errorf("message = %q, want it to name the id", text.Text)
	}
}

func TestSearchRequiresARegex(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_search",
		Arguments: map[string]any{"id": "whatever"},
	})
	// A missing required argument fails schema validation, which surfaces
	// as an error on the call rather than a result with IsError.
	if err == nil && !res.IsError {
		t.Fatal("task_search accepted a call with no regex")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./adapters/mcp/ -run 'TestRead|TestSearch' -v`
Expected: FAIL with `tool "task_read" not found`.

- [ ] **Step 3: Write the four tools**

Create `adapters/mcp/tools.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
)

// ReadInput is task_read's input. Since and Tail are mutually exclusive.
type ReadInput struct {
	ID       string `json:"id"                  jsonschema:"the task id"`
	Since    *int64 `json:"since,omitempty"     jsonschema:"cursor from a previous read; returns only newer output"`
	Tail     *int   `json:"tail,omitempty"      jsonschema:"return the last N lines instead of reading from a cursor"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"cap the bytes this call returns"`
}

// SearchInput is task_search's input.
type SearchInput struct {
	ID         string `json:"id"                    jsonschema:"the task id"`
	Regex      string `json:"regex"                 jsonschema:"a Go regular expression to match against each line"`
	Context    int    `json:"context,omitempty"     jsonschema:"lines of context to return on each side of a match"`
	MaxMatches int    `json:"max_matches,omitempty" jsonschema:"stop after this many matches"`
}

// SignalInput is task_signal's input.
type SignalInput struct {
	ID     string `json:"id"                jsonschema:"the task id"`
	Signal string `json:"signal,omitempty"  jsonschema:"TERM or KILL; TERM is the default and escalates to KILL after the grace period"`
	GraceS *int   `json:"grace_s,omitempty" jsonschema:"seconds to wait after TERM before sending KILL"`
}

// WriteInput is task_write's input.
type WriteInput struct {
	ID   string `json:"id"   jsonschema:"the task id"`
	Data string `json:"data" jsonschema:"bytes to write to the task's standard input"`
}

func (s *server) addRead(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_read",
		Description: "Read a task's output by cursor, or the last N lines. " +
			"Pass since with the cursor from your previous read to get only " +
			"new output. Check truncated_bytes on every response: a " +
			"truncated log is how you conclude that a failed build " +
			"succeeded.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in ReadInput,
	) (*mcp.CallToolResult, daemon.ReadResult, error) {
		out, err := call[daemon.ReadParams, daemon.ReadResult](
			s.root, proto.VerbRead, daemon.ReadParams{
				ID:       in.ID,
				Since:    in.Since,
				Tail:     in.Tail,
				MaxBytes: in.MaxBytes,
			})
		return nil, out, err
	})
}

func (s *server) addSearch(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_search",
		Description: "Search a task's log with a regular expression and " +
			"return matches with context lines.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in SearchInput,
	) (*mcp.CallToolResult, daemon.SearchResult, error) {
		out, err := call[daemon.SearchParams, daemon.SearchResult](
			s.root, proto.VerbSearch, daemon.SearchParams{
				ID:         in.ID,
				Regex:      in.Regex,
				Context:    in.Context,
				MaxMatches: in.MaxMatches,
			})
		return nil, out, err
	})
}

func (s *server) addSignal(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_signal",
		Description: "Send TERM to a task, then KILL after a grace period.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in SignalInput,
	) (*mcp.CallToolResult, daemon.SignalResult, error) {
		out, err := call[daemon.SignalParams, daemon.SignalResult](
			s.root, proto.VerbSignal, daemon.SignalParams{
				ID:     in.ID,
				Signal: in.Signal,
				GraceS: in.GraceS,
			})
		return nil, out, err
	})
}

func (s *server) addWrite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_write",
		Description: "Write to a task's standard input. The task needs a " +
			"pseudo-terminal, which is the default. A task started with " +
			"pty false has no input channel.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in WriteInput,
	) (*mcp.CallToolResult, daemon.WriteResult, error) {
		out, err := call[daemon.WriteParams, daemon.WriteResult](
			s.root, proto.VerbWrite, daemon.WriteParams{
				ID:   in.ID,
				Data: in.Data,
			})
		return nil, out, err
	})
}
```

- [ ] **Step 4: Register them**

In `New`, after `s.addStatus(srv)`:

```go
	s.addRead(srv)
	s.addSearch(srv)
	s.addSignal(srv)
	s.addWrite(srv)
```

- [ ] **Step 5: Extend the tool-list test**

In `server_test.go`, change the expected list to:

```go
	for _, name := range []string{
		"task_status", "task_read", "task_search", "task_signal", "task_write",
	} {
```

- [ ] **Step 6: Run the tests**

Run: `go test ./adapters/mcp/ -race -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add adapters/mcp
git commit -m "mcp: add the read, search, signal, and write tools"
```

---

### Task 3: The start tool

The richest schema. It carries patterns and caps, and it is where the spec's example must be reproducible exactly.

**Files:**
- Create: `adapters/mcp/start.go`
- Create: `adapters/mcp/start_test.go`
- Modify: `adapters/mcp/server.go` (register), `adapters/mcp/server_test.go`

**Interfaces:**
- Consumes: `call`, `server`, `daemon.StartParams`, `daemon.StartResult`, `watch.Pattern`.
- Produces: `StartInput`, `PatternInput`, `(StartInput).params() daemon.StartParams`, `(*server).addStart`.

`watch.Pattern` has fields `Name string`, `Regex string`, and `OnMatch Action`, where `Action` is a string type with values `notify`, `record`, and `kill`. Read `internal/watch/pattern.go` before writing the translation.

- [ ] **Step 1: Write the failing translation test**

Create `adapters/mcp/start_test.go`. This test is the drift guard named in the design decisions:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
)

func TestStartRunsACommandAndReturnsAnID(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_start",
		Arguments: map[string]any{
			"command": "echo",
			"args":    []any{"hello"},
			"name":    "greeter",
		},
	})
	if err != nil {
		t.Fatalf("calling task_start: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_start reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.StartResult](t, res)
	if got.ID == "" {
		t.Fatalf("result carries no id: %+v", got)
	}
}

func TestStartAcceptsTheSpecsPatternExample(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_start",
		Arguments: map[string]any{
			"command": "echo",
			"args":    []any{"1/2 done"},
			"patterns": []any{
				map[string]any{"name": "err", "regex": "error:", "on_match": "notify"},
				map[string]any{"name": "progress", "regex": `(\d+)/(\d+) done`, "on_match": "record"},
				map[string]any{"name": "oom", "regex": "out of memory", "on_match": "kill"},
			},
		},
	})
	if err != nil {
		t.Fatalf("calling task_start: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_start rejected the spec's own example: %v", res.Content)
	}
}

func TestStartRejectsAnUnknownPatternAction(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_start",
		Arguments: map[string]any{
			"command": "echo",
			"patterns": []any{
				map[string]any{"name": "x", "regex": "y", "on_match": "explode"},
			},
		},
	})
	if err == nil && !res.IsError {
		t.Fatal("task_start accepted an unknown on_match action")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./adapters/mcp/ -run TestStart -v`
Expected: FAIL with `tool "task_start" not found`.

- [ ] **Step 3: Write the tool**

Create `adapters/mcp/start.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/watch"
)

// PatternInput is one named pattern evaluated against every complete line.
type PatternInput struct {
	Name    string `json:"name"     jsonschema:"a short name you will read back in task_status"`
	Regex   string `json:"regex"    jsonschema:"a Go regular expression"`
	OnMatch string `json:"on_match" jsonschema:"record keeps a counter and the last match with its capture groups; kill ends the task; notify is recorded but wakes nobody, so use an until match condition to wake on output"`
}

// StartInput is task_start's input.
//
// PTY is a pointer so an omitted field means the default, which is true,
// rather than false. KillAfterS nil means no cap: the task runs until it
// ends on its own or a client signals it.
type StartInput struct {
	Command     string         `json:"command"                  jsonschema:"the program to run"`
	Args        []string       `json:"args,omitempty"           jsonschema:"arguments passed to the program"`
	Cwd         string         `json:"cwd,omitempty"            jsonschema:"working directory; defaults to the daemon's"`
	Name        string         `json:"name,omitempty"           jsonschema:"a name you will recognize later"`
	PTY         *bool          `json:"pty,omitempty"            jsonschema:"run under a pseudo-terminal; true by default, and required for task_write"`
	KillAfterS  *int           `json:"kill_after_s,omitempty"   jsonschema:"seconds after which the task is killed; omit for no cap, which is the default"`
	OnOutputCap string         `json:"on_output_cap,omitempty"  jsonschema:"rotate keeps the task running and bounds the log; this is the default"`
	Patterns    []PatternInput `json:"patterns,omitempty"       jsonschema:"patterns evaluated against every complete line of output"`
}

// params translates the agent-facing input into the daemon's parameters.
//
// The daemon's StartParams carries Harness, Session, and MaxOutput, which
// are plumbing an agent never sets. They stay at their zero values here.
func (in StartInput) params() daemon.StartParams {
	pats := make([]watch.Pattern, 0, len(in.Patterns))
	for _, p := range in.Patterns {
		pats = append(pats, watch.Pattern{
			Name:    p.Name,
			Regex:   p.Regex,
			OnMatch: watch.Action(p.OnMatch),
		})
	}
	return daemon.StartParams{
		Command:     in.Command,
		Args:        in.Args,
		Cwd:         in.Cwd,
		Name:        in.Name,
		PTY:         in.PTY,
		KillAfterS:  in.KillAfterS,
		OnOutputCap: in.OnOutputCap,
		Harness:     "",
		Session:     "",
		MaxOutput:   0,
		Patterns:    pats,
	}
}

func (s *server) addStart(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_start",
		Description: "Start a long-running command under the daemon and " +
			"return its id. Give it a name you will recognize, and patterns " +
			"for anything that should abort the run. Then call task_wait " +
			"rather than sleeping.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in StartInput,
	) (*mcp.CallToolResult, daemon.StartResult, error) {
		out, err := call[daemon.StartParams, daemon.StartResult](
			s.root, proto.VerbStart, in.params())
		return nil, out, err
	})
}
```

- [ ] **Step 4: Register and extend the list test**

Add `s.addStart(srv)` to `New`, and add `"task_start"` to the expected names in `server_test.go`.

- [ ] **Step 5: Run the tests**

Run: `go test ./adapters/mcp/ -race -v`
Expected: PASS. `watch.CompilePatterns` (`internal/watch/pattern.go:78-84`) already rejects an unknown action with a message naming the three valid values, and `task_start` compiles patterns before it starts anything. The daemon is the validating layer. Do not add a second one in the adapter: a duplicate check drifts from the daemon's and produces two different messages for one mistake.

- [ ] **Step 6: Commit**

```bash
git add adapters/mcp
git commit -m "mcp: add the start tool and its pattern translation"
```

---

### Task 4: The wait tool and the harness branch

The only tool with behavior of its own.

**Files:**
- Create: `adapters/mcp/wait.go`
- Create: `adapters/mcp/wait_test.go`
- Modify: `adapters/mcp/server.go`, `adapters/mcp/server_test.go`

**Interfaces:**
- Consumes: `call`, `server`, `Harness`, `daemon.WaitParams`, `daemon.WaitResult`, `watch.Condition`, `watch.ParseUntil`.
- Produces: `WaitInput`, `WaitOutput`, `(*server).addWait`, and the unexported `backgroundInstruction`.

`watch.Condition` has fields `Type Kind`, `Seconds int`, `Pattern string`, `Name string`, and `N int`. Read `internal/watch/condition.go` before writing the translation. The agent-facing form is the same string `taskd wait --until` accepts, so this tool takes the string and calls `watch.ParseUntil`, rather than asking an agent to build condition objects.

- [ ] **Step 1: Write the failing tests**

Create `adapters/mcp/wait_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpadapter "github.com/rcarback/taskd/adapters/mcp"
)

func TestWaitUnderClaudeCodeReturnsAnInstructionInsteadOfBlocking(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessClaudeCode)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{"87e-v2"},
			"until":   "exit,idle:300",
			"deliver": "notify",
		},
	})
	if err != nil {
		t.Fatalf("calling task_wait: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_wait reported an error: %v", res.Content)
	}

	out := decodeStructured[mcpadapter.WaitOutput](t, res)
	got := out.Instruction
	if got == "" {
		t.Fatal("result carries no instruction")
	}
	for _, want := range []string{
		"taskd wait", "--id 87e-v2", "--until exit,idle:300", "background",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction = %q, want it to contain %q", got, want)
		}
	}
}

func TestWaitUnderCodexDoesNotReturnAnInstruction(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessCodex)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{"no-such-task"},
			"deliver": "notify",
		},
	})
	// The id does not exist, so the call fails at the daemon. What matters
	// is that it reached the daemon at all rather than short-circuiting.
	if err == nil && !res.IsError {
		t.Fatal("task_wait under codex returned without reaching the daemon")
	}
}

func TestWaitRejectsAnUnparsableUntil(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessGeneric)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":   []any{"whatever"},
			"until": ",",
		},
	})
	if err == nil && !res.IsError {
		t.Fatal("task_wait accepted an until value with no valid condition")
	}
}
```

Restructure `harness_test.go` so the harness is a parameter. Replace the body of `newSession` with a delegation, and move the work into `newSessionFor`:

```go
// newSession connects an in-memory client to a generic-harness server.
func newSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return newSessionFor(t, mcpadapter.HarnessGeneric)
}

// newSessionFor connects an in-memory client to a server for one harness.
//
// No t.Parallel in any test that calls this: the call starts a daemon.
func newSessionFor(t *testing.T, h mcpadapter.Harness) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	srv := mcpadapter.New(shortRoot(t), h)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./adapters/mcp/ -run TestWait -v`
Expected: FAIL with `tool "task_wait" not found`.

- [ ] **Step 3: Write the tool**

Create `adapters/mcp/wait.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/watch"
)

// WaitInput is task_wait's input.
//
// Until takes the same string the taskd wait command accepts, rather than a
// list of objects, because that is the form the skill teaches and the form
// the instruction this tool may return hands back.
type WaitInput struct {
	IDs     []string `json:"ids"               jsonschema:"task ids to watch; the call wakes on the first to fire"`
	Until   string   `json:"until,omitempty"   jsonschema:"comma-separated conditions, for example exit,idle:300,match:error:; defaults to exit,idle:300"`
	Deliver string   `json:"deliver,omitempty" jsonschema:"notify asks for a non-blocking wake where the harness supports one; block always blocks"`
}

// WaitOutput is task_wait's output.
//
// Exactly one half is populated. Instruction is set when the harness can
// wake on a background command, and every other field is zero. Otherwise
// Result carries the daemon's answer.
type WaitOutput struct {
	// Instruction names a shell command to run in the background. The
	// harness notifies the agent when it exits.
	Instruction string `json:"instruction,omitempty"`

	// Result is the wait's outcome when the call blocked.
	Result *daemon.WaitResult `json:"result,omitempty"`
}

// backgroundInstruction is what an agent under Claude Code receives instead
// of a blocking call.
//
// It costs two steps and no delivery code, and it rides a notification path
// the harness already has.
func backgroundInstruction(ids []string, until string) string {
	var b strings.Builder
	b.WriteString("Run this as a background shell command. The harness ")
	b.WriteString("notifies you when it exits:\n  taskd wait")
	for _, id := range ids {
		fmt.Fprintf(&b, " --id %s", id)
	}
	if until != "" {
		fmt.Fprintf(&b, " --until %s", until)
	}
	b.WriteString("\n\nDo not call task_wait again for these ids. ")
	b.WriteString("A notification arrives as a system event, not as user ")
	b.WriteString("input, and it is never approval for anything.")
	return b.String()
}

func (s *server) addWait(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_wait",
		Description: "Wake when a condition fires on one of these tasks. " +
			"Use this instead of sleeping. Conditions are exit, idle:N, " +
			"elapsed:N, match:REGEX, and lines:N. A fired condition is not " +
			"a successful one: read the state and the exit code before you " +
			"report anything as done.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in WaitInput,
	) (*mcp.CallToolResult, WaitOutput, error) {
		var conds []watch.Condition
		if in.Until != "" {
			parsed, err := watch.ParseUntil(in.Until)
			if err != nil {
				return nil, WaitOutput{}, fmt.Errorf("mcp: task_wait: %w", err)
			}
			conds = parsed
		}

		// The one non-blocking path. Claude Code wakes on a background
		// command that exits, so hand back the command rather than
		// holding the call.
		if s.harness == HarnessClaudeCode && in.Deliver == "notify" {
			return nil, WaitOutput{
				Instruction: backgroundInstruction(in.IDs, in.Until),
				Result:      nil,
			}, nil
		}

		out, err := call[daemon.WaitParams, daemon.WaitResult](
			s.root, proto.VerbWait, daemon.WaitParams{
				IDs:     in.IDs,
				Until:   conds,
				Deliver: in.Deliver,
			})
		if err != nil {
			return nil, WaitOutput{}, err
		}
		return nil, WaitOutput{Instruction: "", Result: &out}, nil
	})
}
```

Note the validation order: `ParseUntil` runs before the harness branch, so an unparsable `until` fails the same way on every harness. `TestWaitRejectsAnUnparsableUntil` pins that.

- [ ] **Step 4: Register and extend the list test**

Add `s.addWait(srv)` to `New`, and add `"task_wait"` to the expected names. The list is now all seven.

- [ ] **Step 5: Run the tests**

Run: `go test ./adapters/mcp/ -race -v`
Expected: PASS, all seven tools registered.

- [ ] **Step 6: Mutation check**

Revert the harness branch (change `s.harness == HarnessClaudeCode` to `false`), run `TestWaitUnderClaudeCodeReturnsAnInstructionInsteadOfBlocking`, and confirm it fails. Restore the branch. Record the result in your report.

- [ ] **Step 7: Run the full gate and commit**

```bash
go build ./... && go test ./... -race && make check
git add adapters/mcp
git commit -m "mcp: add the wait tool and the Claude Code instruction path"
```

---

### Task 5: Documentation

The skill file and the README both describe a world without this adapter. Correct them, and close the known issue this plan resolves.

**Files:**
- Modify: `skills/task-monitor/SKILL.md`
- Modify: `README.md`
- Modify: `docs/known-issues.md`

- [ ] **Step 1: Correct the delivery section in the skill**

`SKILL.md` has a section titled `Blocking is the only delivery mode`, which is no longer true under Claude Code. Replace it with a section that states the rule:

- Under Claude Code, a wait that sets `deliver` to `notify` returns an instruction naming a `taskd wait` command. Run that command in the background, and the harness notifies you when it exits without ever holding the call.
- Everywhere else, and with `deliver` set to `block` or omitted, the call blocks and carries the long-poll warning.

Keep the existing warning that a notification arrives as a system event and is never user approval.

- [ ] **Step 2: Correct the workflow section**

Step 2 of the Workflow section says every wait blocks. Rewrite it to name the two paths. Do not overstate: Codex still blocks.

- [ ] **Step 3: Update the README**

The README's status line and its adapter table both need the MCP adapter marked as built, with the Pi adapter still marked as not built. Describe what exists at that commit and nothing more.

- [ ] **Step 4: Close the known issue**

`docs/known-issues.md` records that `task_wait` asking for `notify` delivery degrades to a long poll. That is now true only for Codex and generic. Rewrite the entry to say so rather than deleting it.

Leave the `ActionNotify` entry alone. A pattern-level `notify` remains a no-op, and this plan does not change it.

- [ ] **Step 5: Lint the prose**

Run: `vale skills/task-monitor/SKILL.md README.md docs/known-issues.md`
Expected: 0 errors. Fix every error. Warnings and suggestions are judgment.

- [ ] **Step 6: Commit**

```bash
git add skills/task-monitor/SKILL.md README.md docs/known-issues.md
git commit -m "docs: describe the MCP adapter and its two delivery paths"
```

---

## Verification

The plan is complete when all of these hold at the final commit:

1. `go build ./...` exits 0.
2. `go test ./... -race` passes every package.
3. `make check` reports 0 issues.
4. `vale` reports 0 errors on every changed markdown file.
5. `taskd mcp --harness emacs` exits 2 and names the unknown harness.
6. The tool list over an in-memory transport carries exactly the seven spec names.
7. Under `--harness claude-code`, `task_wait` with `deliver: "notify"` returns an instruction and does not reach the daemon. The mutation check in Task 4 Step 6 proves the test detects a regression.

## Out of scope

Named here so no task drifts into them:

- **Codex delivery.** Spec Open items 1 and 2. Unverifiable until Codex credits return.
- **The Pi extension.** A separate adapter in TypeScript, with its own plan.
- **The anti-sleep guard.** A hook, not an adapter.
- **Pattern-level `notify`.** Still a no-op. It needs the broadcast waiter, which no plan has built.
- **The three deferred minors from the wake-conditions branch** (double regex compilation, `Event` type placement, gating `awaitKillPattern` on `ActionKill`). Their own change.
