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

	for _, name := range []string{
		"task_status", "task_read", "task_search", "task_signal", "task_write",
	} {
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
