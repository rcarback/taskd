// SPDX-License-Identifier: AGPL-3.0-or-later

// Package paths is the one place that knows where taskd keeps its files.
//
// The daemon, the client, and the tests all resolve the same root through
// this package, so a test can redirect every one of them with one variable.
package paths

import (
	"os"
	"path/filepath"
)

// RootEnv names the variable that overrides the taskd root directory.
const RootEnv = "TASKD_ROOT"

// Root reports the directory that holds the socket, the lock, and every
// task record.
func Root() string {
	if dir := os.Getenv(RootEnv); dir != "" {
		return dir
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return ".taskd"
	}
	return filepath.Join(cache, "taskd")
}

// SocketPath reports the Unix socket the daemon listens on.
func SocketPath(root string) string { return filepath.Join(root, "taskd.sock") }

// LockPath reports the file a client locks before it starts a daemon.
func LockPath(root string) string { return filepath.Join(root, "daemon.lock") }

// DaemonLogPath reports the file a client-spawned daemon writes its stdout
// and stderr to, so a daemon that fails during startup leaves a diagnostic
// behind instead of failing silently.
func DaemonLogPath(root string) string { return filepath.Join(root, "daemon.log") }

// TasksDir reports the directory that holds one subdirectory per task.
func TasksDir(root string) string { return filepath.Join(root, "tasks") }
