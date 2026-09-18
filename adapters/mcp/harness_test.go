// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	mcpadapter "github.com/rcarback/taskd/adapters/mcp"
	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/daemon"
)

// shortRoot returns a daemon root short enough for a Unix socket path.
//
// A macOS sun_path holds 104 bytes and t.TempDir() embeds the test name,
// which overflows it for any test with a descriptive name.
func shortRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "td")
	if err != nil {
		t.Fatalf("creating a root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Clean(dir)
}

// startDaemon runs a real daemon on root until the test ends.
//
// call, and so every tool, reaches the daemon over its Unix socket rather
// than through the mcpadapter server directly, and client.Call only spawns
// one by re-execing this process's own binary — a go test binary, which
// does not understand "serve". Running the daemon in-process here, exactly
// as TestClientCallStartsATaskAndWaitsOnItOverTheSocket in
// internal/daemon/wait_test.go does, sidesteps that re-exec path entirely.
func startDaemon(t *testing.T, root string) {
	t.Helper()
	d, err := daemon.New(root, clock.System())
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	d.Register()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v, want nil after cancellation", err)
		}
	})
}

// newSession connects an in-memory client to a generic-harness server.
func newSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return newSessionFor(t, mcpadapter.HarnessGeneric)
}

// newSessionFor connects an in-memory client to a server for one harness,
// on a fresh root, with a real daemon running behind it.
//
// No t.Parallel in any test that calls this: the call starts a daemon.
func newSessionFor(t *testing.T, h mcpadapter.Harness) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()

	root := shortRoot(t)
	startDaemon(t, root)

	srv := mcpadapter.New(root, h)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })

	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.1"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// decodeStructured re-encodes a tool result's structured content and decodes
// it into T.
//
// CallToolResult.StructuredContent is an any holding whatever the handler
// returned, so a direct type assertion couples every test to the SDK's
// internal representation of that value. A JSON round trip does not.
//
// Shared test helper, used by every tool's tests in this package.
func decodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
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
