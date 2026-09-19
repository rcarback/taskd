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

// TestStartPinsThePatternsNameAndRegexToTheirOwnFields guards against
// params() swapping PatternInput.Name and PatternInput.Regex when it builds
// watch.Pattern. Both orderings produce a working program — patterns still
// compile and the task still starts — so a verb-pinning test cannot catch
// this: the only symptom is a pattern named after its own regular
// expression, watching for text nobody will ever print.
//
// The pattern name and the text the task prints are chosen so neither can
// be mistaken for the other, which makes the two assertions below
// independent: under the swap, the name comes back as the regex and the
// count stays zero, because the task never prints the pattern's name.
func TestStartPinsThePatternsNameAndRegexToTheirOwnFields(t *testing.T) {
	cs := newSession(t)

	const (
		patternName = "totally-unlike-anything-the-task-prints-7q2z"
		printedText = "pattern-match-marker-9f2c"
	)

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "task_start",
		Arguments: map[string]any{
			"command": "echo",
			"args":    []any{printedText},
			"patterns": []any{
				map[string]any{"name": patternName, "regex": printedText, "on_match": "record"},
			},
		},
	})
	if err != nil {
		t.Fatalf("calling task_start: %v", err)
	}
	if res.IsError {
		t.Fatalf("task_start reported an error: %v", res.Content)
	}
	id := decodeStructured[daemon.StartResult](t, res).ID

	waitForExit(t, cs, id)

	statusRes, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "task_status",
		Arguments: map[string]any{"ids": []any{id}},
	})
	if err != nil {
		t.Fatalf("calling task_status: %v", err)
	}
	if statusRes.IsError {
		t.Fatalf("task_status reported an error: %v", statusRes.Content)
	}

	status := decodeStructured[daemon.StatusResult](t, statusRes)
	if len(status.Tasks) != 1 || len(status.Tasks[0].Patterns) != 1 {
		t.Fatalf("status = %+v, want exactly one task with one pattern", status)
	}

	got := status.Tasks[0].Patterns[0]
	if got.Name != patternName {
		t.Errorf("Name = %q, want %q: the pattern must come back under the name it was given, not its regex", got.Name, patternName)
	}
	if got.Count == 0 {
		t.Errorf("Count = 0, want a non-zero match count: the regex must be what the task actually printed, not the pattern's name")
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
