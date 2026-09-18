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
	conn, dialErr := net.Dial("unix", sock)
	if dialErr == nil {
		return conn, nil
	}
	// Only a dial error that actually means "nothing is listening here"
	// justifies unlinking the socket and spawning a daemon. Anything else —
	// permission denied on the socket, say — means a daemon may well be
	// alive and reachable to someone else; unlinking it would strand that
	// daemon on an orphaned inode, unreachable forever, with this client
	// never the wiser. Report the dial failure instead of guessing.
	if !isStaleSocketError(dialErr) {
		return nil, fmt.Errorf("client: dial %s: %w", sock, dialErr)
	}

	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("client: remove stale socket %s: %w", sock, err)
	}
	if err := spawnDaemon(root); err != nil {
		return nil, err
	}
	return waitForSocket(sock)
}

// isStaleSocketError reports whether err from dialing the socket means no
// daemon is listening there — either the file is gone, or it is a leftover
// inode with nothing behind it (ECONNREFUSED). Any other error, such as
// permission denied, says nothing about whether a daemon is alive.
func isStaleSocketError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist)
}

// lockDaemonStart takes an exclusive lock so that concurrent clients start
// exactly one daemon. The returned function releases it.
func lockDaemonStart(root string) (func(), error) {
	path := paths.LockPath(root)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is paths.LockPath(root), not untrusted input
	if err != nil {
		return nil, fmt.Errorf("client: open %s: %w", path, err)
	}
	if err := flock(f, syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("client: lock %s: %w", path, err)
	}
	return func() {
		_ = flock(f, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// flock runs the flock(2) operation op against f's own file descriptor.
//
// f.Fd() alone only returns the descriptor number: nothing ties the
// descriptor's validity to the call using it, so correctness would rest on
// f happening to stay reachable (and so unclosed and un-finalized) for as
// long as that number is in use. SyscallConn's Control method is the
// standard library's documented way to run a raw syscall against a file's
// descriptor, and it keeps the descriptor valid for the duration of the
// callback, so the lock's lifetime is tied to the call rather than to an
// incidental reachability argument.
func flock(f *os.File, op int) error {
	sc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	if err := sc.Control(func(fd uintptr) { opErr = syscall.Flock(int(fd), op) }); err != nil {
		return err
	}
	return opErr
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

	// Give the daemon somewhere to report a startup failure. Without this,
	// a daemon that dies before it opens the socket leaves no trace beyond
	// this client's own "did not listen in time" timeout, which names no
	// cause.
	logPath := paths.DaemonLogPath(root)
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // logPath is paths.DaemonLogPath(root), not untrusted input
	if err != nil {
		return fmt.Errorf("client: open %s: %w", logPath, err)
	}
	defer func() { _ = log.Close() }()

	cmd := exec.Command(bin, "serve", "--root", root) //nolint:gosec // bin is this binary, or a test's override
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("client: start daemon: %w", err)
	}
	// The daemon outlives this client, so release the child handle rather
	// than waiting for it: Release tells the Go runtime never to reap this
	// PID itself. Setsid does not reparent the child to init — that only
	// happens once this client process exits, since setsid changes the
	// child's session, not its parent. Until this client exits, the daemon
	// is still this client's own child, so a daemon that dies first sits as
	// a zombie (harmless, but real) until this client's own exit hands it to
	// init to sweep up. taskd's client is short-lived, so that window closes
	// immediately in practice.
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
		time.Sleep(pollInterval) //nolint:forbidigo // polling a real external process's socket, not a test waiting on a time-based condition — no fake clock makes it start faster
	}
}
