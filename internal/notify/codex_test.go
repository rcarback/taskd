// SPDX-License-Identifier: AGPL-3.0-or-later

package notify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexQueuesOnlyToTheExplicitThread(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "args")
	t.Setenv("NOTIFY_ARGS", output)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NOTIFY_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o700); err != nil { //nolint:gosec // owner-only executable fixture for the fake CLI
		t.Fatal(err)
	}
	thread := "11111111-2222-3333-4444-555555555555"
	if err := Codex(context.Background(), thread, []byte(`{"id":"abc","state":"exited","exit_code":7}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(output) //nolint:gosec // path belongs to this test temporary directory
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, "queue\n--thread\n"+thread+"\n--message\n") || !strings.Contains(got, "not user approval") || !strings.Contains(got, `"exit_code":7`) {
		t.Fatalf("unexpected queue args: %s", got)
	}
}

func TestCodexReportsDeliveryFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\necho queue-failed >&2\nexit 9\n"), 0o700); err != nil { //nolint:gosec // owner-only executable fixture for the fake CLI
		t.Fatal(err)
	}
	err := Codex(context.Background(), "11111111-2222-3333-4444-555555555555", []byte(`{}`))
	if err == nil || !strings.Contains(err.Error(), "queue-failed") {
		t.Fatalf("want delivery error, got %v", err)
	}
}

func TestCodexRejectsThreadBeforeExecuting(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Codex(context.Background(), "latest", nil); err == nil || !strings.Contains(err.Error(), "UUID") {
		t.Fatalf("want thread error, got %v", err)
	}
}
