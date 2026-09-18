// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpadapter "github.com/rcarback/taskd/adapters/mcp"
	"github.com/rcarback/taskd/internal/daemon"
)

// buildTaskdBinary compiles the real taskd binary so a test can drive
// "taskd mcp" as an actual subprocess speaking MCP over its own stdio.
//
// This is the only way to observe whether --harness reaches the adapter:
// every adapter test builds a server in-process with a harness it chose
// directly (mcpadapter.New(root, h)), which never touches runMCP or the
// flag parsing in this package at all.
func buildTaskdBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "taskd")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/rcarback/taskd/cmd/taskd") //nolint:gosec // fixed args, bin is this test's own t.TempDir() path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// mcpTestRoot returns a taskd root short enough for the daemon's Unix
// socket path. t.TempDir() embeds the full test name, which overflows
// macOS's 104-byte sun_path for a descriptively named test.
func mcpTestRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// stopMCPTestDaemon ends the daemon that "taskd mcp" spawned against root.
// The subprocess itself exits when the client session closes, but the
// daemon it started with client.Call detaches into its own session and
// outlives it.
func stopMCPTestDaemon(t *testing.T, root string) {
	t.Helper()
	_ = exec.Command("pkill", "-f", "taskd serve --root "+root).Run() //nolint:gosec // root is this test's own mcpTestRoot(t) path
}

// mcpDecodeStructured re-encodes a tool result's structured content and
// decodes it into T. CallToolResult.StructuredContent is an any holding
// whatever the handler returned, so a direct type assertion couples the
// test to the SDK's internal representation of that value.
func mcpDecodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
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

// TestMCPHarnessFlagReachesTheAdapter drives "taskd mcp --harness
// claude-code" as a real subprocess and calls task_wait with deliver:
// notify.
//
// Only a server built with HarnessClaudeCode answers that call with an
// instruction instead of contacting the daemon; every other harness blocks
// and returns a Result. That difference is the one observable effect of
// cmd/taskd/main.go:118 actually passing the parsed --harness flag into
// mcpadapter.New, rather than some fixed harness, so it is what this test
// pins. A mutation that drops the flag on the floor makes this test fail
// while leaving every adapter-package test green, because those tests
// choose their harness directly and never go through this command at all.
func TestMCPHarnessFlagReachesTheAdapter(t *testing.T) {
	bin := buildTaskdBinary(t)
	root := mcpTestRoot(t)
	t.Cleanup(func() { stopMCPTestDaemon(t, root) })

	ctx := context.Background()
	transport := &mcp.CommandTransport{
		Command: exec.Command(bin, "mcp", "--root", root, "--harness", "claude-code"), //nolint:gosec // bin is this test's own buildTaskdBinary(t) path, root is mcpTestRoot(t)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connecting to taskd mcp: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	startRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "task_start",
		Arguments: map[string]any{"command": "true"},
	})
	if err != nil {
		t.Fatalf("calling task_start: %v", err)
	}
	if startRes.IsError {
		t.Fatalf("task_start reported an error: %v", startRes.Content)
	}
	id := mcpDecodeStructured[daemon.StartResult](t, startRes).ID
	if id == "" {
		t.Fatal("task_start returned no id")
	}

	waitRes, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name: "task_wait",
		Arguments: map[string]any{
			"ids":     []any{id},
			"deliver": "notify",
		},
	})
	if err != nil {
		t.Fatalf("calling task_wait: %v", err)
	}
	if waitRes.IsError {
		t.Fatalf("task_wait reported an error: %v", waitRes.Content)
	}

	out := mcpDecodeStructured[mcpadapter.WaitOutput](t, waitRes)
	if out.Instruction == "" {
		t.Fatalf("task_wait under \"taskd mcp --harness claude-code\" returned no "+
			"instruction (result = %+v); the --harness flag did not reach the adapter", out.Result)
	}
	if !strings.Contains(out.Instruction, "taskd wait") {
		t.Errorf("instruction = %q, want it to name the taskd wait command", out.Instruction)
	}
	if !strings.Contains(out.Instruction, "--id "+id) {
		t.Errorf("instruction = %q, want it to name the started task's id %q", out.Instruction, id)
	}
}
