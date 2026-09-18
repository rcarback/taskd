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
