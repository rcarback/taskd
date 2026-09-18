// SPDX-License-Identifier: AGPL-3.0-or-later

package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootHonorsTheEnvironmentOverride(t *testing.T) {
	t.Setenv(RootEnv, "/tmp/taskd-test-root")
	if got := Root(); got != "/tmp/taskd-test-root" {
		t.Fatalf("Root() = %q, want the override", got)
	}
}

func TestRootFallsBackToTheCacheDirectory(t *testing.T) {
	t.Setenv(RootEnv, "")
	got := Root()
	if got == "" {
		t.Fatal("Root() = empty")
	}
	if !strings.HasSuffix(got, "taskd") {
		t.Fatalf("Root() = %q, want a path ending in taskd", got)
	}
}

func TestDerivedPathsSitUnderTheRoot(t *testing.T) {
	root := "/tmp/r"
	for name, got := range map[string]string{
		"socket": SocketPath(root),
		"lock":   LockPath(root),
		"tasks":  TasksDir(root),
	} {
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Fatalf("%s path %q is not under %q", name, got, root)
		}
	}
}
