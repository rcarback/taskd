// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
//
// runMCP passes two values into mcpadapter.New: root and harness. Both are
// pinned here, not just harness: the emitted instruction names the root the
// server was actually given, so a mutation that points the adapter at a
// different root — one that leaves every other test in the repository
// green, because nothing else asserts which root a real "taskd mcp"
// process used — fails this test too.
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
	if !strings.Contains(out.Instruction, "--root '"+root+"'") {
		t.Errorf("instruction = %q, want it to name the --root flag's own root %q: "+
			"the --root flag did not reach the adapter", out.Instruction, root)
	}
	if !strings.Contains(out.Instruction, "--id "+id) {
		t.Errorf("instruction = %q, want it to name the started task's id %q", out.Instruction, id)
	}
}

func TestCodexWaitQueuesTheRealTaskResultAndReportsDeliveryFailure(t *testing.T) {
	bin := buildTaskdBinary(t)
	root := mcpTestRoot(t)
	t.Cleanup(func() { stopMCPTestDaemon(t, root) })
	ctx := context.Background()
	transport := &mcp.CommandTransport{Command: exec.Command(bin, "mcp", "--root", root, "--harness", "codex")} //nolint:gosec // binary and arguments come from this test, without shell interpolation
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	started, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "task_start", Arguments: map[string]any{"command": "sh", "args": []string{"-c", "exit 7"}}})
	if err != nil || started.IsError {
		t.Fatalf("start: %v %+v", err, started)
	}
	id := mcpDecodeStructured[daemon.StartResult](t, started).ID
	thread := "11111111-2222-3333-4444-555555555555"
	waited, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "task_wait", Arguments: map[string]any{"ids": []string{id}, "until": "exit", "deliver": "notify", "thread_id": thread}})
	if err != nil || waited.IsError {
		t.Fatalf("arm: %v %+v", err, waited)
	}
	instruction := mcpDecodeStructured[mcpadapter.WaitOutput](t, waited).Instruction
	if !strings.Contains(instruction, "--notify-thread "+thread) {
		t.Fatal(instruction)
	}
	fake := t.TempDir()
	received := filepath.Join(fake, "received")
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NOTIFY_ARGS", received)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NOTIFY_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte(script), 0o700); err != nil { //nolint:gosec // owner-only executable fixture for the fake CLI
		t.Fatal(err)
	}
	command := func() *exec.Cmd {
		return exec.Command(bin, "wait", "--root", root, "--id", id, "--until", "exit", "--notify-thread", thread) //nolint:gosec // binary and arguments come from this test, without shell interpolation
	}
	output, err := command().CombinedOutput()
	if err != nil {
		t.Fatalf("wait: %v %s", err, output)
	}
	message, err := os.ReadFile(received) //nolint:gosec // path belongs to this test temporary directory
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"queue\n--thread\n" + thread, "not user approval", `"exit_code":7`, id} {
		if !strings.Contains(string(message), want) {
			t.Errorf("missing %s in %s", want, message)
		}
	}
	failed := exec.Command(bin, "wait", "--root", root, "--id", "missing-task", "--until", "exit", "--notify-thread", thread) //nolint:gosec // binary and arguments come from this test, without shell interpolation
	output, err = failed.CombinedOutput()
	if err == nil {
		t.Fatalf("missing task unexpectedly succeeded: %s", output)
	}
	message, err = os.ReadFile(received) //nolint:gosec // path belongs to this test temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(message), "wait_error") || !strings.Contains(string(message), "missing-task") {
		t.Fatalf("wait failure was not queued: %s", message)
	}
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte("#!/bin/sh\necho delivery-refused >&2\nexit 9\n"), 0o700); err != nil { //nolint:gosec // owner-only executable fixture for the fake CLI
		t.Fatal(err)
	}
	output, err = command().CombinedOutput()
	if err == nil || !strings.Contains(string(output), "delivery-refused") {
		t.Fatalf("delivery failure hidden: %v %s", err, output)
	}
}

func TestDetachedWaitReturnsAtOnceAndDeliversFromItsOwnSession(t *testing.T) {
	bin := buildTaskdBinary(t)
	root := mcpTestRoot(t)
	t.Cleanup(func() { stopMCPTestDaemon(t, root) })
	ctx := context.Background()
	transport := &mcp.CommandTransport{Command: exec.Command(bin, "mcp", "--root", root, "--harness", "codex")} //nolint:gosec // binary and arguments come from this test, without shell interpolation
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	started, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "task_start", Arguments: map[string]any{"command": "sh", "args": []string{"-c", "sleep 3; exit 7"}}})
	if err != nil || started.IsError {
		t.Fatalf("start: %v %+v", err, started)
	}
	id := mcpDecodeStructured[daemon.StartResult](t, started).ID

	fake := t.TempDir()
	received := filepath.Join(fake, "received")
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NOTIFY_ARGS", received)
	// A FIFO: the read below returns when the detached waiter's queue call
	// writes and closes it.
	if err := syscall.Mkfifo(received, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NOTIFY_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(fake, "codex"), []byte(script), 0o700); err != nil { //nolint:gosec // owner-only executable fixture for the fake CLI
		t.Fatal(err)
	}
	delivered := make(chan []byte, 1)
	go func() {
		message, _ := os.ReadFile(received) //nolint:gosec // path belongs to this test temporary directory
		delivered <- message
	}()

	// The log is a file, as in the instruction. A pipe would stay open in the
	// detached child and hold this call until the task ended.
	logPath := filepath.Join(fake, "wait.log")
	logFile, err := os.Create(logPath) //nolint:gosec // path belongs to this test temporary directory
	if err != nil {
		t.Fatal(err)
	}
	thread := "11111111-2222-3333-4444-555555555555"
	cmd := exec.Command(bin, "wait", "--detach", "--root", root, "--id", id, "--until", "exit", "--notify-thread", thread) //nolint:gosec // binary and arguments come from this test, without shell interpolation
	cmd.Stdout, cmd.Stderr = logFile, logFile
	begun := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("wait --detach: %v", err)
	}
	_ = logFile.Close()
	if took := time.Since(begun); took > 2*time.Second {
		t.Fatalf("wait --detach took %s, want it to return before the 3 s task ends", took)
	}
	logged, err := os.ReadFile(logPath) //nolint:gosec // path belongs to this test temporary directory
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(logged), "taskd: detached waiter %d", &pid); err != nil {
		t.Fatalf("no waiter pid in %q: %v", logged, err)
	}
	// ps reads the session on both Linux and macOS, where syscall has no Getsid.
	sess, err := exec.Command("ps", "-o", "sess=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // pid is the waiter this test started
	if err != nil || strings.TrimSpace(string(sess)) != strconv.Itoa(pid) {
		t.Fatalf("waiter %d has session %q (%v), want it to lead its own session", pid, sess, err)
	}

	var message []byte
	select {
	case message = <-delivered:
	case <-time.After(time.Minute):
		t.Fatal("the detached waiter queued nothing within a minute")
	}
	for _, want := range []string{"queue\n--thread\n" + thread, `"exit_code":7`, id} {
		if !strings.Contains(string(message), want) {
			t.Errorf("missing %s in %s", want, message)
		}
	}
}

func TestDetachWithoutANotifyThreadIsAUsageError(t *testing.T) {
	var out strings.Builder
	if code := dispatch([]string{"wait", "--id", "abc", "--detach"}, &out); code != 2 {
		t.Fatalf("exit = %d, want 2: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "--detach needs --notify-thread") {
		t.Fatalf("output = %q", out.String())
	}
}
