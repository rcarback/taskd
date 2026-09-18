// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemon runs the taskd supervisor process: it owns every task and
// answers client requests on a Unix socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

// Handler answers one verb. It returns the value to encode into
// Response.Result, or an error to report in Response.Error.
type Handler func(params json.RawMessage) (any, error)

// Daemon owns every task on this machine for this user.
type Daemon struct {
	Root string
	Clk  clock.Clock
	Reg  *Registry

	ln    net.Listener
	conns *connSet

	mu       sync.RWMutex
	handlers map[proto.Verb]Handler
}

// New prepares the taskd root, listens on the socket, reconciles records
// left by a previous daemon, and returns.
//
// New listens rather than Serve, so a caller knows the socket exists as soon
// as New returns and a test never races the accept loop.
//
// Owning the socket comes before reconcile on purpose: clearStaleSocket
// below is what detects a daemon still holding this root. Reconciling first
// would rewrite a live daemon's running records to lost before that daemon
// was ever discovered, corrupting state that still belongs to it.
func New(root string, clk clock.Clock) (*Daemon, error) {
	if err := os.MkdirAll(paths.TasksDir(root), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", root, err)
	}
	// MkdirAll only sets the mode on a directory it creates, so a root left
	// over with looser permissions (or created by something other than
	// taskd) would otherwise keep them. The plan's mode guarantee is
	// unconditional, not just for a fresh root.
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // 0700 is a directory mode: the execute bit is required for traversal, and this is the plan's own directory mode constraint
		return nil, fmt.Errorf("daemon: chmod %s: %w", root, err)
	}

	sock := paths.SocketPath(root)
	if err := clearStaleSocket(sock); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen on %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("daemon: chmod %s: %w", sock, err)
	}

	if err := reconcile(root, clk); err != nil {
		_ = ln.Close()
		return nil, err
	}

	return &Daemon{
		Root:     root,
		Clk:      clk,
		Reg:      NewRegistry(),
		ln:       ln,
		conns:    newConnSet(),
		handlers: map[proto.Verb]Handler{},
	}, nil
}

// reconcile marks every task that a previous daemon left running as lost.
//
// When the daemon dies its children die with it, and their exit codes die
// with them. Reporting lost is the honest answer and keeps a restart
// correct without re-adopting orphaned processes.
func reconcile(root string, clk clock.Clock) error {
	records, err := record.Scan(root)
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.State != supervisor.StateRunning {
			continue
		}
		r.State = supervisor.StateLost
		ended := clk.Now()
		r.EndedAt = &ended
		if err := record.Save(taskDir(root, r.ID), r); err != nil {
			return err
		}
	}
	return nil
}

// clearStaleSocket removes a socket file that no daemon is listening on.
//
// A live daemon answers a dial, so a successful dial means this process must
// not take the address. Anything else means the file is left over from a
// daemon that died, and Listen would fail on it.
func clearStaleSocket(sock string) error {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("daemon: another daemon is already listening on %s", sock)
	}
	if err := os.Remove(sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale socket %s: %w", sock, err)
	}
	return nil
}

// taskDir reports the directory holding one task's record and log.
func taskDir(root, id string) string {
	return filepath.Join(paths.TasksDir(root), id)
}

// Addr reports the socket path the daemon listens on.
func (d *Daemon) Addr() string { return d.ln.Addr().String() }

// Handle registers h for v, replacing any previous handler.
func (d *Daemon) Handle(v proto.Verb, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[v] = h
}

// Serve accepts connections until ctx ends, then returns nil.
//
// Shutdown is bounded rather than left to chance: ending ctx closes the
// listener so no new connection is accepted, and closes every connection
// currently open so a client that dialed and never wrote (or a handler
// deliberately holding a connection open, as a future long poll will)
// cannot keep this call blocked forever. stop lets the same cleanup run
// when Serve returns for a reason other than ctx ending — a permanent
// Accept error — so that path closes the listener and every open
// connection too, rather than leaking them along with the watcher
// goroutine below.
func (d *Daemon) Serve(ctx context.Context) error {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(stop)

	go func() {
		select {
		case <-ctx.Done():
		case <-stop:
		}
		_ = d.ln.Close()
		d.conns.closeAll()
	}()

	for {
		conn, err := d.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daemon: accept: %w", err)
		}
		if !d.conns.add(conn) {
			// closeAll already ran: shutdown started between Accept
			// returning this connection and it being registered. No later
			// closeAll call will reach it, so close it here instead.
			_ = conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer d.conns.remove(conn)
			d.serveConn(conn)
		}()
	}
}

// serveConn reads one request, answers it, and closes the connection.
//
// A failure to read the request is the client's problem and cannot be
// reported to it, so the connection simply closes. A failure in the handler
// is reported in the response.
func (d *Daemon) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var req proto.Request
	if err := proto.ReadMessage(conn, &req); err != nil {
		return
	}
	_ = proto.WriteMessage(conn, d.safeAnswer(req))
}

// safeAnswer runs answer, converting a panic inside a handler into an error
// response instead of letting it unwind out of this goroutine. Task 4
// registers no handler, but every later task's Handler runs arbitrary code
// against caller-supplied params; a panic in one request must not take
// down the process and every task it supervises along with it.
func (d *Daemon) safeAnswer(req proto.Request) (res proto.Response) {
	defer func() {
		if r := recover(); r != nil {
			res = proto.Response{OK: false, Error: fmt.Sprintf("daemon: handler panic: %v", r)}
		}
	}()
	return d.answer(req)
}

// answer runs the handler for req and builds the response.
func (d *Daemon) answer(req proto.Request) proto.Response {
	d.mu.RLock()
	h, ok := d.handlers[req.Verb]
	d.mu.RUnlock()

	if !ok {
		return proto.Response{OK: false, Error: fmt.Sprintf("daemon: unknown verb %q", req.Verb)}
	}
	result, err := h(req.Params)
	if err != nil {
		return proto.Response{OK: false, Error: err.Error()}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return proto.Response{OK: false, Error: fmt.Sprintf("daemon: encode result: %v", err)}
	}
	return proto.Response{OK: true, Result: b}
}

// connSet tracks connections Serve has accepted but not yet finished
// answering, so shutdown can close them itself rather than waiting on a
// client or a deadline.
type connSet struct {
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// newConnSet returns an empty, open connSet.
func newConnSet() *connSet {
	return &connSet{conns: map[net.Conn]struct{}{}}
}

// add registers conn. It reports false, without registering conn, once
// closeAll has already run: no later closeAll call will reach it, so the
// caller must close conn itself in that case.
func (s *connSet) add(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

// remove drops conn once its handling goroutine has finished with it.
func (s *connSet) remove(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// closeAll closes every currently open connection and marks the set
// closed, so any add from this point on refuses and the caller closes the
// connection itself instead.
func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for conn := range s.conns {
		_ = conn.Close()
	}
	s.conns = nil
}
