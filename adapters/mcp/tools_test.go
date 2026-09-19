// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
)

// waitForExit polls task_status until the task leaves the running state.
//
// adapters/mcp has no direct handle on the daemon's registry the way
// internal/daemon's own tests do, so this waits on the condition the
// daemon exposes over the protocol — the reported state — rather than on
// the clock: no time.Sleep, just a poll loop bounded by a deadline.
func waitForExit(t *testing.T, cs *mcp.ClientSession, id string) daemon.StatusEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "task_status",
			Arguments: map[string]any{"ids": []any{id}},
		})
		if err != nil {
			t.Fatalf("calling task_status: %v", err)
		}
		if res.IsError {
			t.Fatalf("task_status reported an error: %v", res.Content)
		}
		out := decodeStructured[daemon.StatusResult](t, res)
		if len(out.Tasks) != 1 {
			t.Fatalf("task_status returned %d tasks, want 1", len(out.Tasks))
		}
		if out.Tasks[0].State != "running" {
			return out.Tasks[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not reach a terminal state")
		}
	}
}

// startTask calls task_start and returns the new task's id.
func startTask(t *testing.T, cs *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_start",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("calling task_start: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_start reported an error: %v", res.Content)
	}
	return decodeStructured[daemon.StartResult](t, res).ID
}

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

// TestReadReturnsTheTasksOwnOutput is a success-path test for task_read.
//
// It asserts on the task's own output, not merely that Data is non-empty,
// so a task_read wired to another verb's result decodes into a zero
// ReadResult and this assertion fails: see the sibling tests for
// task_search, task_signal, and task_write for the same guard on their own
// verbs.
func TestReadReturnsTheTasksOwnOutput(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "echo",
		"args":    []any{"read-marker-19a4"},
	})
	waitForExit(t, cs, id)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_read",
		Arguments: map[string]any{"id": id},
	})
	if err != nil {
		t.Fatalf("calling task_read: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_read reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.ReadResult](t, res)
	if !strings.Contains(got.Data, "read-marker-19a4") {
		t.Fatalf("Data = %q, want it to contain the task's own output", got.Data)
	}
}

// TestSearchFindsALineInTheTasksOutput is a success-path test for
// task_search.
func TestSearchFindsALineInTheTasksOutput(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", `printf 'first\nsearch-marker-77f2\nlast\n'`},
	})
	waitForExit(t, cs, id)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_search",
		Arguments: map[string]any{"id": id, "regex": "search-marker-77f2"},
	})
	if err != nil {
		t.Fatalf("calling task_search: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_search reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.SearchResult](t, res)
	if len(got.Matches) == 0 {
		t.Fatal("Matches is empty, want at least one hit")
	}
	if !strings.Contains(got.Matches[0].Line, "search-marker-77f2") {
		t.Fatalf("Matches[0].Line = %q, want it to contain the search term", got.Matches[0].Line)
	}
}

// TestSignalDefaultsToTheDaemonsSignal is a success-path test for
// task_signal. It checks the default the daemon applies when the caller
// names no signal, rather than assuming it, per internal/daemon/verbs.go's
// signal handler.
func TestSignalDefaultsToTheDaemonsSignal(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{"command": "cat"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_signal",
		Arguments: map[string]any{"id": id},
	})
	if err != nil {
		t.Fatalf("calling task_signal: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_signal reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.SignalResult](t, res)
	want := syscall.SIGTERM.String()
	if got.Signal != want {
		t.Fatalf("Signal = %q, want %q, the daemon's default", got.Signal, want)
	}
}

// TestWriteReportsTheBytesItSent is a success-path test for task_write. The
// byte count can only be correct if the Data field it sent actually reached
// the daemon's write handler, so a shape-only assertion would not catch a
// tool wired to another verb.
func TestWriteReportsTheBytesItSent(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{"command": "cat"})

	data := "hello\n"
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_write",
		Arguments: map[string]any{"id": id, "data": data},
	})
	if err != nil {
		t.Fatalf("calling task_write: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_write reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.WriteResult](t, res)
	if got.Written != len(data) {
		t.Fatalf("Written = %d, want %d", got.Written, len(data))
	}
}
