// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
)

// This file pins the fields each tool mirrors into its own verb's params,
// the half of the translation layer the verb-pinning tests in the other
// files in this package do not reach. Every test here fails if the field
// under test is dropped on the way to the daemon, even though the call
// still reaches the right verb and the result still decodes into the right
// type — the gap eight surviving mutations found in the final review.

// TestStartNameReachesTheDaemon pins task_start's name field. Both skill
// files and the task_start description lead with name, and nothing else in
// this package proves it reaches the daemon.
func TestStartNameReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	const name = "translation-test-name-8f2c"
	id := startTask(t, cs, map[string]any{"command": "true", "name": name})

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
	if out.Tasks[0].Name != name {
		t.Errorf("Name = %q, want %q", out.Tasks[0].Name, name)
	}
}

// TestStartCwdReachesTheDaemon pins task_start's cwd field, using a task
// that prints its own working directory.
func TestStartCwdReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	dir := t.TempDir()
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	id := startTask(t, cs, map[string]any{"command": "pwd", "cwd": dir})
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
	if strings.TrimSpace(got.Data) != canon {
		t.Errorf("pwd printed %q, want the task's cwd %q", strings.TrimSpace(got.Data), canon)
	}
}

// TestStartPTYFalseReachesTheDaemon pins task_start's pty field. A task
// started with pty false has no input channel, per task_write's own
// description, so task_write against it fails only if pty false actually
// reached the daemon; the default is true, so a dropped field would leave
// the write succeeding.
func TestStartPTYFalseReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{"command": "cat", "pty": false})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_write",
		Arguments: map[string]any{"id": id, "data": "hello\n"},
	})
	if err != nil {
		t.Fatalf("calling task_write: %v", err)
	}
	if !res.IsError {
		t.Fatal("task_write succeeded against a task started with pty false")
	}
}

// TestStartKillAfterSReachesTheDaemon pins task_start's kill_after_s field.
// The task busy-loops forever on its own, so it only reaches the killed
// state if the cap actually reached the daemon.
func TestStartKillAfterSReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command":      "sh",
		"args":         []any{"-c", "while :; do :; done"},
		"kill_after_s": 1,
	})

	got := waitForExit(t, cs, id)
	if got.State != "killed" {
		t.Errorf("State = %q, want %q", got.State, "killed")
	}
}

// TestSignalGraceSReachesTheDaemon pins task_signal's grace_s field. The
// task ignores SIGTERM, so it only reaches the killed state within the
// test's short deadline if the daemon insisted with SIGKILL after the
// caller's own grace period rather than the ten-second default.
func TestSignalGraceSReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", `trap "" TERM; while :; do :; done`},
	})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_signal",
		Arguments: map[string]any{"id": id, "grace_s": 0},
	})
	if err != nil {
		t.Fatalf("calling task_signal: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_signal reported an error: %v", res.Content)
	}

	got := waitForExit(t, cs, id)
	if got.State != "killed" {
		t.Errorf("State = %q, want %q: grace_s: 0 should have insisted almost at once", got.State, "killed")
	}
}

// TestSignalFieldReachesTheDaemon pins task_signal's signal field itself,
// distinct from grace_s: SignalResult.Signal echoes back the syscall the
// daemon actually chose, so a KILL request that reaches the daemon reports
// KILL, and a dropped field would default to TERM regardless of what the
// caller asked for. Minor 16's fix advertised that signal admits KILL
// directly; this is what proves the advertised capability works.
func TestSignalFieldReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{"command": "cat"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_signal",
		Arguments: map[string]any{"id": id, "signal": "KILL"},
	})
	if err != nil {
		t.Fatalf("calling task_signal: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_signal reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.SignalResult](t, res)
	want := syscall.SIGKILL.String()
	if got.Signal != want {
		t.Errorf("Signal = %q, want %q: signal did not reach the daemon", got.Signal, want)
	}
}

// TestStatusIDsReachesTheDaemon pins task_status's ids field, distinct from
// listing everything: with two tasks in the root, a call naming only one id
// must return exactly that one, not both. A dropped ids field would fall
// back to task_status's own "no ids means list everything" behavior.
func TestStatusIDsReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id1 := startTask(t, cs, map[string]any{"command": "cat"})
	startTask(t, cs, map[string]any{"command": "cat"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_status",
		Arguments: map[string]any{"ids": []any{id1}},
	})
	if err != nil {
		t.Fatalf("calling task_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_status reported an error: %v", res.Content)
	}

	out := decodeStructured[daemon.StatusResult](t, res)
	if len(out.Tasks) != 1 {
		t.Fatalf("task_status returned %d tasks, want 1: ids did not reach the daemon", len(out.Tasks))
	}
	if out.Tasks[0].ID != id1 {
		t.Errorf("Tasks[0].ID = %q, want %q", out.Tasks[0].ID, id1)
	}
}

// TestStartOnOutputCapReachesTheDaemon pins task_start's on_output_cap
// field. The daemon rejects any value other than "rotate", the only cap
// behavior implemented, naming the caller's own value in the error; a
// dropped field would default to "rotate" regardless of what was sent and
// the call would succeed instead.
func TestStartOnOutputCapReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_start",
		Arguments: map[string]any{"command": "true", "on_output_cap": "bogus-cap-3f8a"},
	})
	if err != nil {
		t.Fatalf("calling task_start: %v", err)
	}
	if !res.IsError {
		t.Fatal("task_start accepted an on_output_cap value the daemon does not implement")
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "bogus-cap-3f8a") {
		t.Errorf("message = %q, want it to name the on_output_cap value", text.Text)
	}
}

// TestReadSinceReachesTheDaemon pins task_read's since field. A second read
// at the cursor the first read returned must come back empty, because
// nothing wrote more output in between; a dropped since field would default
// to cursor 0 and repeat the first call's data.
func TestReadSinceReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "echo first-read-marker-3c9a"},
	})
	waitForExit(t, cs, id)

	first := readTask(t, cs, map[string]any{"id": id})
	if !strings.Contains(first.Data, "first-read-marker-3c9a") {
		t.Fatalf("first read Data = %q, want it to contain the task's output", first.Data)
	}

	second := readTask(t, cs, map[string]any{"id": id, "since": first.Next})
	if second.Data != "" {
		t.Errorf("second read at the first read's cursor returned %q, want empty", second.Data)
	}
}

// TestReadTailReachesTheDaemon pins task_read's tail field.
func TestReadTailReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "printf 'tail-marker-first\\ntail-marker-second\\ntail-marker-third\\n'"},
	})
	waitForExit(t, cs, id)

	got := readTask(t, cs, map[string]any{"id": id, "tail": 1})
	if strings.Contains(got.Data, "tail-marker-first") {
		t.Errorf("Data = %q, want tail: 1 to exclude the earlier lines", got.Data)
	}
	if !strings.Contains(got.Data, "tail-marker-third") {
		t.Errorf("Data = %q, want it to contain the last line", got.Data)
	}
}

// TestReadMaxBytesReachesTheDaemon pins task_read's max_bytes field. The
// default read cap is 64KiB, far larger than the task's own output, so a
// dropped max_bytes would return everything the task printed rather than a
// handful of bytes.
func TestReadMaxBytesReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "for i in 1 2 3 4 5 6 7 8 9 10; do echo max-bytes-marker-line; done"},
	})
	waitForExit(t, cs, id)

	got := readTask(t, cs, map[string]any{"id": id, "max_bytes": 5})
	if len(got.Data) > 5 {
		t.Errorf("Data is %d bytes, want at most 5: max_bytes did not reach the daemon", len(got.Data))
	}
}

// readTask calls task_read and decodes its result.
func readTask(t *testing.T, cs *mcp.ClientSession, args map[string]any) daemon.ReadResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_read",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("calling task_read: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_read reported an error: %v", res.Content)
	}
	return decodeStructured[daemon.ReadResult](t, res)
}

// TestSearchContextReachesTheDaemon pins task_search's context field.
func TestSearchContextReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "printf 'context-marker-before\\ncontext-marker-hit\\ncontext-marker-after\\n'"},
	})
	waitForExit(t, cs, id)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_search",
		Arguments: map[string]any{
			"id": id, "regex": "context-marker-hit", "context": 1,
		},
	})
	if err != nil {
		t.Fatalf("calling task_search: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_search reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.SearchResult](t, res)
	if len(got.Matches) != 1 {
		t.Fatalf("Matches has %d entries, want 1", len(got.Matches))
	}
	m := got.Matches[0]
	if len(m.Before) != 1 || !strings.Contains(m.Before[0], "context-marker-before") {
		t.Errorf("Before = %v, want one line containing %q: context did not reach the daemon", m.Before, "context-marker-before")
	}
	if len(m.After) != 1 || !strings.Contains(m.After[0], "context-marker-after") {
		t.Errorf("After = %v, want one line containing %q: context did not reach the daemon", m.After, "context-marker-after")
	}
}

// TestSearchMaxMatchesReachesTheDaemon pins task_search's max_matches
// field, distinct from context: a swap between the two would still pass a
// test that only checks one of them.
func TestSearchMaxMatchesReachesTheDaemon(t *testing.T) {
	cs := newSession(t)

	id := startTask(t, cs, map[string]any{
		"command": "sh",
		"args":    []any{"-c", "printf 'max-matches-marker\\nmax-matches-marker\\nmax-matches-marker\\n'"},
	})
	waitForExit(t, cs, id)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_search",
		Arguments: map[string]any{
			"id": id, "regex": "max-matches-marker", "max_matches": 1,
		},
	})
	if err != nil {
		t.Fatalf("calling task_search: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_search reported an error: %v", res.Content)
	}

	got := decodeStructured[daemon.SearchResult](t, res)
	if len(got.Matches) != 1 {
		t.Fatalf("Matches has %d entries, want 1: max_matches did not reach the daemon", len(got.Matches))
	}
	if !got.More {
		t.Error("More = false, want true: a third match exists beyond max_matches")
	}
}
