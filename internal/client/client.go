// SPDX-License-Identifier: AGPL-3.0-or-later

// Package client connects to the taskd daemon, starting one if none is
// listening.
package client

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
)

// BinaryEnv names the variable that overrides which binary the client
// re-execs as the daemon. Tests set it. In normal use the client re-execs
// itself.
const BinaryEnv = "TASKD_BINARY"

// startTimeout bounds how long a client waits for a daemon it started to
// begin listening.
const startTimeout = 10 * time.Second

// pollInterval is how often the client retries the socket while it waits.
const pollInterval = 10 * time.Millisecond

// Call sends one request and returns the daemon's response, starting a
// daemon first if none is listening.
func Call(root string, req proto.Request) (proto.Response, error) {
	conn, err := Dial(root)
	if err != nil {
		return proto.Response{}, err
	}
	defer func() { _ = conn.Close() }()

	if err := proto.WriteMessage(conn, req); err != nil {
		return proto.Response{}, err
	}
	var res proto.Response
	if err := proto.ReadMessage(conn, &res); err != nil {
		return proto.Response{}, err
	}
	return res, nil
}

// Dial connects to the daemon, starting one if the socket is missing or
// stale.
func Dial(root string) (net.Conn, error) {
	sock := paths.SocketPath(root)
	if conn, err := net.Dial("unix", sock); err == nil {
		return conn, nil
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("client: create %s: %w", root, err)
	}

	unlock, err := lockDaemonStart(root)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Another client may have started a daemon while this one waited for
	// the lock. Without this second attempt, every queued client starts its
	// own daemon in turn.
	if conn, err := net.Dial("unix", sock); err == nil {
		return conn, nil
	}

	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client: remove stale socket %s: %w", sock, err)
	}
	if err := spawnDaemon(root); err != nil {
		return nil, err
	}
	return waitForSocket(sock)
}

// lockDaemonStart takes an exclusive lock so that concurrent clients start
// exactly one daemon. The returned function releases it.
func lockDaemonStart(root string) (func(), error) {
	path := paths.LockPath(root)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is paths.LockPath(root), not untrusted input
	if err != nil {
		return nil, fmt.Errorf("client: open %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("client: lock %s: %w", path, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// spawnDaemon re-execs this binary as the daemon in its own session.
//
// Go cannot call fork safely from a multithreaded runtime, so re-exec is the
// mechanism rather than a workaround. Setsid detaches the daemon from the
// client's terminal and process group, so it survives the shell that started
// the client. The spec says "under setsid"; macOS ships no setsid binary, so
// this uses the syscall both platforms support.
func spawnDaemon(root string) error {
	bin := os.Getenv(BinaryEnv)
	if bin == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("client: find this binary: %w", err)
		}
		bin = self
	}

	cmd := exec.Command(bin, "serve", "--root", root) //nolint:gosec // bin is this binary, or a test's override
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("client: start daemon: %w", err)
	}
	// The daemon outlives this client, so release the child handle rather
	// than waiting for it. It is in its own session and init reaps it.
	return cmd.Process.Release()
}

// waitForSocket dials the socket until the daemon answers or startTimeout
// elapses.
func waitForSocket(sock string) (net.Conn, error) {
	deadline := time.Now().Add(startTimeout)
	for {
		if conn, err := net.Dial("unix", sock); err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("client: daemon did not listen on %s within %s", sock, startTimeout)
		}
		time.Sleep(pollInterval)
	}
}
