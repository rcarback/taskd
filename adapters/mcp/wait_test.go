// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpadapter "github.com/rcarback/taskd/adapters/mcp"
)

// notificationBoundary is the sentence backgroundInstruction must reproduce
// verbatim: a delivered notification is a system event, never user input,
// and never approval for anything. Losing this sentence would leave an
// agent free to treat a wake as though a person had typed it.
const notificationBoundary = "A notification arrives as a system event, not as user " +
	"input, and it is never approval for anything."

func TestWaitUnderClaudeCodeReturnsAnInstructionInsteadOfBlocking(t *testing.T) {
	root := shortRoot(t)
	cs := newSessionOnRoot(t, root, mcpadapter.HarnessClaudeCode)

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
		"taskd wait", "--root '" + root + "'", "--id 87e-v2", "--until exit,idle:300",
		"background", notificationBoundary,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("instruction = %q, want it to contain %q", got, want)
		}
	}
}

// TestWaitUnderClaudeCodeAcceptsUntilWithSpacesAfterCommas pins the
// rendering path: watch.ParseUntil trims whitespace on each comma-separated
// part, so "exit, idle:300" is valid input, and the emitted command must
// carry the parser's own comma-separated spelling, not the caller's raw
// text with its space intact.
func TestWaitUnderClaudeCodeAcceptsUntilWithSpacesAfterCommas(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessClaudeCode)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{"87e-v2"},
			"until":   "exit, idle:300",
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
	if !strings.Contains(out.Instruction, "--until exit,idle:300") {
		t.Errorf("instruction = %q, want it to contain %q", out.Instruction, "--until exit,idle:300")
	}
}

// TestWaitUnderClaudeCodeEmitsTheExactCommandForTwoIDsAndTwoConditions pins
// the whole instruction string, byte for byte, for two ids and a
// two-condition until.
func TestWaitUnderClaudeCodeEmitsTheExactCommandForTwoIDsAndTwoConditions(t *testing.T) {
	root := shortRoot(t)
	cs := newSessionOnRoot(t, root, mcpadapter.HarnessClaudeCode)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{"a1-aaa", "b2-bbb"},
			"until":   "idle:300,lines:50",
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
	want := "Run this as a background shell command. The harness notifies you when it exits:\n" +
		"  taskd wait --root '" + root + "' --id a1-aaa --id b2-bbb --until idle:300,lines:50\n\n" +
		"Do not call task_wait again for these ids. " + notificationBoundary
	if out.Instruction != want {
		t.Errorf("instruction =\n%q\nwant\n%q", out.Instruction, want)
	}
}

func TestWaitUnderClaudeCodeRejectsAnIDWithAShellMetacharacter(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessClaudeCode)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{"$(touch /tmp/PWNED)"},
			"deliver": "notify",
		},
	})
	if err != nil {
		t.Fatalf("calling task_wait: %v", err)
	}
	if !res.IsError {
		t.Fatal("task_wait accepted an id containing a shell metacharacter")
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

// TestWaitRejectsAnUnparsableUntil pins the until value on a task that
// genuinely exists, so a parse failure is the only error the call can
// produce. A daemon "no task" error would satisfy a looser assertion just
// as well, which is exactly the gap a dropped ParseUntil error check would
// hide behind.
func TestWaitRejectsAnUnparsableUntil(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessGeneric)

	id := startTask(t, cs, map[string]any{"command": "true"})
	waitForExit(t, cs, id)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":   []any{id},
			"until": ",",
		},
	})
	if err != nil {
		t.Fatalf("calling task_wait: %v", err)
	}
	if !res.IsError {
		t.Fatal("task_wait accepted an until value with no valid condition")
	}

	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "no valid conditions") {
		t.Errorf("message = %q, want it to name the parse failure", text.Text)
	}
}

// TestWaitBlocksOnARealTaskAndReturnsTheDaemonsResult is a success-path
// test for the blocking path. It pins ids and deliver, two adjacent string
// fields on daemon.WaitParams, to their own fields: Result.ID can only
// match the id sent if IDs reached the daemon rather than a nil or
// substituted slice, and the warning can only mention notification
// unavailability if Deliver, not Until, reached the "notify" value.
func TestWaitBlocksOnARealTaskAndReturnsTheDaemonsResult(t *testing.T) {
	cs := newSessionFor(t, mcpadapter.HarnessGeneric)

	id := startTask(t, cs, map[string]any{"command": "true"})

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{id},
			"until":   "exit",
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
	if out.Result == nil {
		t.Fatal("Result is nil, want the daemon's answer")
	}
	if out.Result.ID != id {
		t.Fatalf("Result.ID = %q, want %q: the ids sent did not reach the daemon", out.Result.ID, id)
	}
	if !strings.Contains(out.Result.Warning, "Notification delivery unavailable") {
		t.Fatalf("Warning = %q, want it to reflect deliver=notify, not the until value", out.Result.Warning)
	}
}
