// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rcarback/taskd/internal/supervisor"
)

func TestRunReportsExitCodeAndWritesLog(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer

	code := run([]string{"--root", root, "--no-pty", "--", "sh", "-c", "echo marker; exit 3"}, &stdout)

	if code != 3 {
		t.Fatalf("run returned %d, want 3", code)
	}
	if !strings.Contains(stdout.String(), "exited") {
		t.Fatalf("stdout = %q, want it to mention the state", stdout.String())
	}

	logs, err := filepath.Glob(filepath.Join(root, "tasks", "*", "out.log"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("found %d logs, want 1", len(logs))
	}
	body, err := os.ReadFile(logs[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(body), "marker") {
		t.Fatalf("log = %q, want it to contain marker", body)
	}
}

func TestRunRejectsAMissingCommand(t *testing.T) {
	if code := run([]string{"--root", t.TempDir(), "--"}, &bytes.Buffer{}); code == 0 {
		t.Fatal("run returned 0 for a missing command")
	}
}

func TestRunReportsSignaledExitCode(t *testing.T) {
	root := t.TempDir()
	var stdout bytes.Buffer

	// The child sends itself SIGTERM, so the task ends StateSignaled rather
	// than exiting normally. A signaled task's ExitCode carries no meaning;
	// run must not report it as success.
	code := run([]string{"--root", root, "--no-pty", "--", "sh", "-c", "kill -TERM $$"}, &stdout)

	want := 128 + int(syscall.SIGTERM)
	if code != want {
		t.Fatalf("run returned %d, want %d (128+SIGTERM)", code, want)
	}
	if !strings.Contains(stdout.String(), "signaled") {
		t.Fatalf("stdout = %q, want it to mention the signaled state", stdout.String())
	}
}

func TestDispatchRoutesTheRunSubcommand(t *testing.T) {
	var stdout bytes.Buffer

	code := dispatch([]string{"run", "--root", t.TempDir(), "--no-pty", "--", "sh", "-c", "exit 4"}, &stdout)

	if code != 4 {
		t.Fatalf("dispatch returned %d, want 4", code)
	}
}

func TestDispatchRejectsAnUnknownSubcommand(t *testing.T) {
	var stdout bytes.Buffer

	code := dispatch([]string{"frobnicate"}, &stdout)

	if code != 2 {
		t.Fatalf("dispatch returned %d, want 2", code)
	}
	if !strings.Contains(stdout.String(), "usage:") {
		t.Fatalf("stdout = %q, want usage text", stdout.String())
	}
}

func TestExitCodeForSignaledReturns128PlusSignal(t *testing.T) {
	res := supervisor.Result{State: supervisor.StateSignaled, Signal: syscall.SIGKILL}
	if got, want := exitCode(res), 128+int(syscall.SIGKILL); got != want {
		t.Fatalf("exitCode(%+v) = %d, want %d", res, got, want)
	}
}

func TestExitCodeForExitedReturnsTheChildsCode(t *testing.T) {
	res := supervisor.Result{State: supervisor.StateExited, ExitCode: 42}
	if got := exitCode(res); got != 42 {
		t.Fatalf("exitCode(%+v) = %d, want 42", res, got)
	}
}

func TestReportResultReportsAnIncompleteLogSeparatelyFromExitStatus(t *testing.T) {
	var stdout bytes.Buffer
	res := supervisor.Result{State: supervisor.StateExited, ExitCode: 0, OutputErr: errors.New("disk full")}

	reportResult(&stdout, "task-1", res, 12)

	out := stdout.String()
	if !strings.Contains(out, "exit=0") {
		t.Fatalf("stdout = %q, want the exit line to keep the child's exit code", out)
	}
	if !strings.Contains(out, "disk full") {
		t.Fatalf("stdout = %q, want a line reporting the incomplete capture and its cause", out)
	}
}

func TestReportResultOmitsTheIncompleteLogWhenOutputErrIsNil(t *testing.T) {
	var stdout bytes.Buffer
	res := supervisor.Result{State: supervisor.StateExited, ExitCode: 0}

	reportResult(&stdout, "task-1", res, 12)

	if strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("stdout = %q, want exactly one line when OutputErr is nil", stdout.String())
	}
}
