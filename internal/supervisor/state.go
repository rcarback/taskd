// SPDX-License-Identifier: AGPL-3.0-or-later

// Package supervisor owns task processes and reports how they ended.
package supervisor

import (
	"syscall"
	"time"
)

// State is the lifecycle state of a task.
type State string

// The task states. Every state except StateRunning is terminal.
const (
	StateRunning  State = "running"
	StateExited   State = "exited"
	StateSignaled State = "signaled"
	StateKilled   State = "killed"
	StateFailed   State = "failed"
	StateLost     State = "lost"
)

// Result describes how a task ended.
//
// Callers must read State before ExitCode. A task that ended on a signal has
// no meaningful exit code.
type Result struct {
	State       State
	ExitCode    int
	Signal      syscall.Signal
	Started     time.Time
	Ended       time.Time
	MaxRSSBytes int64

	// OutputErr is non-nil when copying the task's output into the sink
	// failed, so the captured log is incomplete. The process status in
	// State and ExitCode is still accurate.
	OutputErr error
}
