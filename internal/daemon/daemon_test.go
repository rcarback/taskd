// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

var errBoom = errors.New("boom")

// shortRoot returns a taskd root with a short name, under $TMPDIR like
// t.TempDir() but without the test function's name in the path.
//
// t.TempDir() nests the full test name and a run counter under the temp
// directory, and this package appends "/taskd.sock" to the root for the
// socket. Several of this package's own test names are long enough that the
// combination exceeds macOS's 104-byte sockaddr_un.sun_path limit, which
// fails the bind with an unhelpful "invalid argument" unrelated to the
// daemon's own correctness. A short, random suffix keeps every socket path
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

// start runs a daemon on a temporary root and returns it with its socket
// path. It stops the daemon when the test ends.
func start(t *testing.T) (*Daemon, string) {
	t.Helper()
	root := shortRoot(t)
	d, err := New(root, clock.System())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v, want nil after cancellation", err)
		}
	})
	return d, paths.SocketPath(root)
}

// roundTrip sends one request and returns the response.
func roundTrip(t *testing.T, sock string, req proto.Request) proto.Response {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := proto.WriteMessage(conn, req); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	var res proto.Response
	if err := proto.ReadMessage(conn, &res); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	return res
}

func TestDaemonAnswersARegisteredVerb(t *testing.T) {
	d, sock := start(t)
	d.Handle(proto.VerbStatus, func(json.RawMessage) (any, error) {
		return map[string]string{"hello": "world"}, nil
	})

	res := roundTrip(t, sock, proto.Request{Verb: proto.VerbStatus})
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
	if string(res.Result) != `{"hello":"world"}` {
		t.Fatalf("Result = %s, want the handler's value", res.Result)
	}
}

func TestDaemonRejectsAnUnknownVerb(t *testing.T) {
	_, sock := start(t)

	res := roundTrip(t, sock, proto.Request{Verb: "task_nonsense"})
	if res.OK {
		t.Fatal("OK = true for an unknown verb")
	}
	if res.Error == "" {
		t.Fatal("Error is empty for an unknown verb")
	}
}

func TestDaemonReportsAHandlerError(t *testing.T) {
	d, sock := start(t)
	d.Handle(proto.VerbStatus, func(json.RawMessage) (any, error) {
		return nil, errBoom
	})

	res := roundTrip(t, sock, proto.Request{Verb: proto.VerbStatus})
	if res.OK {
		t.Fatal("OK = true when the handler failed")
	}
	if res.Error != errBoom.Error() {
		t.Fatalf("Error = %q, want %q", res.Error, errBoom.Error())
	}
}

func TestDaemonSurvivesAMalformedRequest(t *testing.T) {
	_, sock := start(t)

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := conn.Write([]byte("this is not json\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = conn.Close()

	// The same daemon must still answer the next client on the same socket.
	res := roundTrip(t, sock, proto.Request{Verb: "task_nonsense"})
	if res.OK {
		t.Fatal("OK = true for an unknown verb")
	}
	if res.Error == "" {
		t.Fatal("the daemon answered with no error text after a malformed request")
	}
}

func TestDaemonSocketIsPrivate(t *testing.T) {
	_, sock := start(t)

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}
}

func TestNewMarksAnOrphanedRunningTaskLost(t *testing.T) {
	root := shortRoot(t)
	dir := filepath.Join(paths.TasksDir(root), "orphan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := record.Save(dir, record.Record{
		ID:        "orphan",
		State:     supervisor.StateRunning,
		StartedAt: time.Unix(0, 0),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := New(root, clock.System()); err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := record.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != supervisor.StateLost {
		t.Fatalf("State = %q, want %q: the daemon that owned it is gone", got.State, supervisor.StateLost)
	}
}

func TestNewLeavesAFinishedRecordAlone(t *testing.T) {
	root := shortRoot(t)
	dir := filepath.Join(paths.TasksDir(root), "done")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	code := 3
	if err := record.Save(dir, record.Record{ID: "done", State: supervisor.StateExited, Exit: &code}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if _, err := New(root, clock.System()); err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := record.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.State != supervisor.StateExited || got.Exit == nil || *got.Exit != 3 {
		t.Fatalf("record changed to %+v, want the finished record untouched", got)
	}
}
