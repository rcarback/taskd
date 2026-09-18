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
	"syscall"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/paths"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/record"
	"github.com/rcarback/taskd/internal/supervisor"
)

// Handler answers one verb. ctx ends when the client disconnects or the
// daemon shuts down, which is what releases a long poll that would
// otherwise hold a connection goroutine forever. It returns the value to
// encode into Response.Result, or an error to report in Response.Error.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Daemon owns every task on this machine for this user.
type Daemon struct {
	Root string
	Clk  clock.Clock
	Reg  *Registry

	ln    net.Listener
	conns *connSet
	// lock is held for the whole life of this daemon and proves no other
	// daemon owns this root. Serve releases it where it closes the listener.
	lock *os.File

	mu       sync.RWMutex
	handlers map[proto.Verb]Handler
}

// New prepares the taskd root, listens on the socket, reconciles records
// left by a previous daemon, and returns.
//
// New listens rather than Serve, so a caller knows the socket exists as soon
// as New returns and a test never races the accept loop.
//
// Taking the root lock comes first, and it is what carries the one-daemon-
// per-root invariant. A dial probe cannot carry it: clearStaleSocket unlinks
// the socket path, and unlinking a bound Unix socket and binding a fresh one
// leaves both listeners alive, so two daemons that probe before either binds
// both proceed and the second unlinks the first's socket out from under it.
// Both would then reconcile the same records and allocate task directories
// under the same tree. With the lock held across clearStaleSocket, Listen,
// and reconcile, the only process that can sit between the unlink and the
// bind is the one holding the lock.
//
// Owning the socket still comes before reconcile: reconciling first would
// rewrite a live daemon's running records to lost before that daemon was
// ever discovered, corrupting state that still belongs to it.
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
	// The same unconditional guarantee for the tasks tree. The 0700 parent
	// blocks traversal either way, so this is defence in depth rather than an
	// exposure it closes.
	if err := os.Chmod(paths.TasksDir(root), 0o700); err != nil { //nolint:gosec // see above: a directory mode, and the plan's own constraint
		return nil, fmt.Errorf("daemon: chmod %s: %w", paths.TasksDir(root), err)
	}

	lock, err := lockRoot(root)
	if err != nil {
		return nil, err
	}

	sock := paths.SocketPath(root)
	if err := clearStaleSocket(sock); err != nil {
		_ = lock.Close()
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("daemon: listen on %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		_ = ln.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("daemon: chmod %s: %w", sock, err)
	}

	if err := reconcile(root, clk); err != nil {
		_ = ln.Close()
		_ = lock.Close()
		return nil, err
	}

	return &Daemon{
		Root:     root,
		Clk:      clk,
		Reg:      NewRegistry(),
		ln:       ln,
		conns:    newConnSet(),
		lock:     lock,
		handlers: map[proto.Verb]Handler{},
	}, nil
}

// lockRoot takes the exclusive, non-blocking flock that makes this process
// the one daemon for root. The returned file must stay open: closing it
// releases the lock.
//
// The lock is advisory and tied to the open file description, so the kernel
// releases it when the holder dies. A daemon that crashes frees its root with
// no reaper and no stale-PID logic.
//
// The raw syscall runs through SyscallConn rather than against f.Fd(): Fd
// returns only a descriptor number, and nothing ties that number's validity
// to the call using it, while Control keeps the descriptor valid for the
// duration of the callback. internal/client's own flock helper does the same
// for the client's separate lock, and carries the longer note.
func lockRoot(root string) (*os.File, error) {
	path := paths.DaemonLockPath(root)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is paths.DaemonLockPath(root), not untrusted input
	if err != nil {
		return nil, fmt.Errorf("daemon: open %s: %w", path, err)
	}
	sc, err := f.SyscallConn()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}
	var opErr error
	if err := sc.Control(func(fd uintptr) {
		opErr = syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	}); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: lock %s: %w", path, err)
	}
	if opErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon: another daemon already owns %s: %w", root, opErr)
	}
	return f, nil
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
//
// This is a second opinion, not the proof of sole ownership: the root lock
// New holds before calling this is what carries that invariant. The probe
// still earns its place by naming a reachable daemon in the error rather than
// reporting only that the lock was taken.
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
//
// The watcher is in wg, so Serve does not return until it has finished. The
// defer ordering makes that safe: close(stop) runs before wg.Wait(), so the
// watcher is already released when Serve waits for it. Without the join, the
// watcher could still be between close(stop) and ln.Close after Serve
// returned, and a caller that immediately built a new daemon on the same root
// would find the old listener still bound and the root lock still held.
func (d *Daemon) Serve(ctx context.Context) error {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(stop)

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case <-stop:
		}
		_ = d.ln.Close()
		d.conns.closeAll()
		// Releasing the root lock last: every path that could still touch
		// this root — an in-flight handler on a connection closeAll just
		// closed — is on its way out, and the next daemon must not take the
		// root until the listener is unbound.
		_ = d.lock.Close()
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
			// A per-connection context, derived from Serve's own: shutdown
			// cancels it like every other connection here, and it is also
			// cancelled the moment this goroutine returns. serveConn derives
			// a further child from it that additionally ends when the peer
			// hangs up mid-request, which is what lets a future long poll
			// notice and return instead of holding this goroutine forever.
			connCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			d.serveConn(connCtx, conn)
		}()
	}
}

// serveConn reads one request, answers it, and closes the connection.
//
// A failure to read the request is the client's problem and cannot be
// reported to it, so the connection simply closes. A failure in the handler
// is reported in the response.
//
// A failure to write the response usually means the answer itself is over the
// protocol's size cap. Dropping it closed the connection with nothing on it
// and left the client holding "proto: decode: EOF", which names neither the
// cap nor the verb. WriteMessage rejects an oversize message before it writes
// any of it, so the connection is still clean and one short error response
// fits where the real answer did not. That second write can fail too — a
// client that already hung up — and there is nothing further to say then.
//
// While the handler runs, a second goroutine keeps reading from conn to
// notice the peer hanging up: the protocol carries exactly one message in
// each direction, and ReadMessage above already consumed it, so nothing
// legitimate arrives here again. This is the only way a blocking handler —
// a long poll's task_wait — learns that its caller is gone, since nothing
// else on this connection watches the socket while the handler is in
// flight. Reading and writing the same net.Conn from different goroutines
// at once is safe: the net package documents Conn's methods as callable
// concurrently.
func (d *Daemon) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	var req proto.Request
	if err := proto.ReadMessage(conn, &req); err != nil {
		return
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	hungUp := make(chan struct{})
	go func() {
		defer close(hungUp)
		readUntilHangUp(conn)
		cancel()
	}()
	defer func() {
		// Force the Read blocked inside readUntilHangUp to return once
		// this function is done with conn, so that goroutine does not
		// outlive the connection. SetReadDeadline is safe to call
		// concurrently with the Read it targets — see the doc comment
		// above — and a deadline in the past makes every current and
		// future Read on conn fail at once, which is what lets this wait
		// finish promptly rather than for as long as conn happens to stay
		// open.
		_ = conn.SetReadDeadline(time.Now())
		<-hungUp
	}()

	res := d.safeAnswer(reqCtx, req)
	if reqCtx.Err() != nil {
		// The peer hung up while the handler ran, or the daemon started
		// shutting down: either way nobody is left to read a response, and
		// conn may already be half or fully closed. Writing one here could
		// only fail, the same as any other write to a hung-up client (see
		// the comment on that failure path below) — skip it instead of
		// building a response nobody will see.
		return
	}
	if err := proto.WriteMessage(conn, res); err != nil {
		_ = proto.WriteMessage(conn, proto.Response{
			OK:    false,
			Error: fmt.Sprintf("daemon: cannot send the %s response: %v", req.Verb, err),
		})
	}
}

// readUntilHangUp blocks until a Read on conn fails, then returns.
//
// A successful read is not this connection's peer sending something real —
// the protocol carries exactly one message in each direction — so it is, at
// most, a delimiter byte proto.ReadMessage's decoder did not happen to
// consume from the socket already (in practice it always does: the decoder
// reads whatever the kernel currently has queued, and the client's request
// and its trailing newline arrive from one Write call before the client
// ever reads a response, so they are queued together). Discarding a
// successful read and continuing, rather than treating it as the signal,
// keeps this correct even if that assumption ever stops holding, instead of
// silently giving up on hang-up detection for the rest of the request.
func readUntilHangUp(conn net.Conn) {
	buf := make([]byte, 1)
	for {
		if _, err := conn.Read(buf); err != nil {
			return
		}
	}
}

// safeAnswer runs answer, converting a panic inside a handler into an error
// response instead of letting it unwind out of this goroutine. Task 4
// registers no handler, but every later task's Handler runs arbitrary code
// against caller-supplied params; a panic in one request must not take
// down the process and every task it supervises along with it.
func (d *Daemon) safeAnswer(ctx context.Context, req proto.Request) (res proto.Response) {
	defer func() {
		if r := recover(); r != nil {
			res = proto.Response{OK: false, Error: fmt.Sprintf("daemon: handler panic: %v", r)}
		}
	}()
	return d.answer(ctx, req)
}

// answer runs the handler for req and builds the response.
func (d *Daemon) answer(ctx context.Context, req proto.Request) proto.Response {
	d.mu.RLock()
	h, ok := d.handlers[req.Verb]
	d.mu.RUnlock()

	if !ok {
		return proto.Response{OK: false, Error: fmt.Sprintf("daemon: unknown verb %q", req.Verb)}
	}
	result, err := h(ctx, req.Params)
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
