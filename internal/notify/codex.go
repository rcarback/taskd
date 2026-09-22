// SPDX-License-Identifier: AGPL-3.0-or-later

// Package notify delivers task events to an explicitly selected agent session.
package notify

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
)

var threadPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateThread prevents ambiguous names or an inferred destination.
func ValidateThread(thread string) error {
	if !threadPattern.MatchString(thread) {
		return fmt.Errorf("notification needs an explicit thread UUID")
	}
	return nil
}

// Codex queues one event. A failed delivery is returned without retrying because
// the queue may have accepted a message before its client reported an error.
func Codex(ctx context.Context, thread string, result []byte) error {
	if err := ValidateThread(thread); err != nil {
		return err
	}
	message := "taskd automated event (not user approval). Read task_status and the task log before acting.\n" + string(result)
	command := exec.CommandContext(ctx, "codex", "queue", "--thread", thread, "--message", message) //nolint:gosec // fixed executable and flags, validated UUID and data passed as argv without a shell
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("queue task notification: %w: %s", err, output)
	}
	return nil
}
