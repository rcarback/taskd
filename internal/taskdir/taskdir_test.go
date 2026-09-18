// SPDX-License-Identifier: AGPL-3.0-or-later

package taskdir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rcarback/taskd/internal/paths"
)

func TestNewCreatesAUniqueDirectory(t *testing.T) {
	root := t.TempDir()

	firstDir, firstID, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secondDir, secondID, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if firstID == secondID {
		t.Fatalf("two calls produced the same id %q", firstID)
	}
	for _, dir := range []string{firstDir, secondDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("Stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Fatalf("%s has mode %o, want 700", dir, got)
		}
	}
	if filepath.Dir(firstDir) != filepath.Join(root, "tasks") {
		t.Fatalf("parent = %s, want %s", filepath.Dir(firstDir), filepath.Join(root, "tasks"))
	}
}

func TestNewAgreesWithPathsTasksDir(t *testing.T) {
	t.Setenv(paths.RootEnv, filepath.Join(t.TempDir(), "root"))
	root := paths.Root()

	dir, _, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if want := paths.TasksDir(root); filepath.Dir(dir) != want {
		t.Fatalf("parent = %s, want %s: taskdir and paths disagree on the tasks directory", filepath.Dir(dir), want)
	}
}

func TestIDsSortByCreationOrder(t *testing.T) {
	root := t.TempDir()
	_, first, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, second, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if first >= second {
		t.Fatalf("ids %q and %q do not sort by creation order", first, second)
	}
}
