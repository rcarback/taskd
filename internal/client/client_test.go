// SPDX-License-Identifier: AGPL-3.0-or-later

package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
)

// shortRoot returns a taskd root with a short name, under $TMPDIR like
// t.TempDir() but without the test function's name in the path.
//
// t.TempDir() nests the full test name and a run counter under the temp
// directory, and this package's Dial appends "/taskd.sock" to the root for
// the socket. This package's own test names are long enough that the
// combination exceeds macOS's 104-byte sockaddr_un.sun_path limit, which
// fails the bind with an unhelpful "invalid argument" unrelated to the
// client's own correctness. A short, random suffix keeps every socket path
// well under that limit while still honoring $TMPDIR.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// buildTaskd compiles the binary once per test run so the client has a real
// program to re-exec.
func buildTaskd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "taskd")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/rcarback/taskd/cmd/taskd") //nolint:gosec // fixed args, bin is this test's own t.TempDir() path
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestCallReachesARunningDaemon(t *testing.T) {
	root := shortRoot(t)
	d, err := daemon.New(root, clock.System())
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	d.Handle(proto.VerbStatus, func(json.RawMessage) (any, error) {
		return map[string]int{"n": 1}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	res, err := Call(root, proto.Request{Verb: proto.VerbStatus})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
}

// TestDialDoesNotUnlinkASocketFailingWithANonStaleError guards against
// Dial treating every dial failure as "no daemon here". A socket that a live
// daemon is listening on, but that this client cannot reach for some other
// reason (permission denied, here), must not be unlinked: doing so would
// strand that daemon on an orphaned inode, unreachable to anyone, and spawn
// a second daemon on top of it.
func TestDialDoesNotUnlinkASocketFailingWithANonStaleError(t *testing.T) {
	root := shortRoot(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	sock := paths.SocketPath(root)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// A live listener that no one may connect to: every dial against it
	// fails with permission denied, not with the "nothing is listening"
	// error that means a socket is genuinely stale.
	if err := os.Chmod(sock, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}

	if _, err := Dial(root); err == nil {
		t.Fatal("Dial succeeded against a permission-denied socket")
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket was removed on a non-stale dial error: %v", err)
	}
}

func TestCallStartsADaemonWhenNoneIsListening(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(paths.RootEnv, root)
	t.Setenv(BinaryEnv, buildTaskd(t))
	// Registered before Call runs, not after its assertions: Call can start
	// a daemon and still leave this test to fail an assertion below, and a
	// cleanup registered only after those assertions would never run, and
	// leak that daemon.
	t.Cleanup(func() { stopDaemon(t, root) })

	// The daemon this starts registers no verb handlers of its own — Tasks 6
	// to 8 add them — so task_status necessarily comes back as an unknown
	// verb. That the daemon answered at all, rather than the call erroring
	// out, is what this test is checking: the socket exists and a real
	// daemon is listening on it.
	res, err := Call(root, proto.Request{Verb: proto.VerbStatus})
	if err != nil {
		t.Fatalf("Call on a cold root: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "unknown verb") {
		t.Fatalf("res = %+v, want an unknown-verb error from the unhandled daemon", res)
	}
	if _, err := os.Stat(paths.SocketPath(root)); err != nil {
		t.Fatalf("no socket after Call: %v", err)
	}
}

func TestConcurrentClientsStartExactlyOneDaemon(t *testing.T) {
	root := shortRoot(t)
	t.Setenv(paths.RootEnv, root)
	t.Setenv(BinaryEnv, buildTaskd(t))
	t.Cleanup(func() { stopDaemon(t, root) })

	const clients = 8
	var wg sync.WaitGroup
	errs := make([]error, clients)
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = Call(root, proto.Request{Verb: proto.VerbStatus})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("client %d: %v", i, err)
		}
	}
	if n := countDaemons(t, root); n != 1 {
		t.Fatalf("%d daemons running, want exactly 1", n)
	}
}

// countDaemons counts taskd serve processes started against this test's
// root. The root is a fresh temporary directory, so the match cannot catch
// a daemon belonging to another test or to the developer's own machine.
func countDaemons(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", "taskd serve --root "+root).Output() //nolint:gosec // root is this test's own shortRoot(t) path
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return 0 // pgrep reports "no match" with status 1
		}
		t.Skipf("pgrep unavailable, cannot count daemons: %v", err)
	}
	return len(strings.Fields(string(out)))
}

// stopDaemon ends the daemon this test started.
func stopDaemon(t *testing.T, root string) {
	t.Helper()
	_ = exec.Command("pkill", "-f", "taskd serve --root "+root).Run() //nolint:gosec // root is this test's own shortRoot(t) path
}
