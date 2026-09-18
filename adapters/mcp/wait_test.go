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
