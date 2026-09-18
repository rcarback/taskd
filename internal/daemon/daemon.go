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

	ln net.Listener

	mu       sync.RWMutex
	handlers map[proto.Verb]Handler
}

// New prepares the taskd root, reconciles records left by a previous daemon,
// and listens on the socket.
//
// New listens rather than Serve, so a caller knows the socket exists as soon
// as New returns and a test never races the accept loop.
func New(root string, clk clock.Clock) (*Daemon, error) {
	if err := os.MkdirAll(paths.TasksDir(root), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", root, err)
	}
	if err := reconcile(root, clk); err != nil {
		return nil, err
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

	return &Daemon{
		Root:     root,
		Clk:      clk,
		Reg:      NewRegistry(),
		ln:       ln,
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
func (d *Daemon) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = d.ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := d.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daemon: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
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
	_ = proto.WriteMessage(conn, d.answer(req))
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
