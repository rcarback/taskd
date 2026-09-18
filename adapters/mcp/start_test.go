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
