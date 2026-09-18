// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"

	"github.com/rcarback/taskd/internal/clock"
)

// ptyDrainGrace bounds how long reap waits, after cmd.Wait has already
// returned, for the pty copy goroutine to finish on its own. A grandchild
// that inherited the pty slave and outlived the child can hold it open
// indefinitely, which would otherwise hang Task.Wait forever. By the time
// cmd.Wait returns, the child's own output is already fully in the pty
// buffer, so the grace only protects against that lingering grandchild
// descriptor, never against losing output.
const ptyDrainGrace = 2 * time.Second

// ptyStream is the pty side of a running task: the master end, a channel
// that closes once the copy goroutine returns, and the first error (if any)
// writing the child's output into the sink.
type ptyStream struct {
	master *os.File
	done   chan struct{}

	mu       sync.Mutex
	writeErr error
}

// writeError reports the first error writing the child's output into sink,
// or nil. It is guarded by a mutex rather than relying on the done channel's
// happens-before edge, because drain can return before the copy goroutine
// has finished (see drain).
func (s *ptyStream) writeError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

// drain waits for the copy goroutine to finish, bounded by ptyDrainGrace on
// clk. See drainWithGrace for what happens when the grace expires.
func (s *ptyStream) drain(clk clock.Clock) {
	drainWithGrace(clk, s.done, func() { _ = s.master.Close() })
}

// drainWithGrace waits for done to close, bounded by ptyDrainGrace on clk.
// If the grace elapses first, it calls onGraceExpired — which, for a real
// ptyStream, closes the master to force its pending read to fail so the
// copy goroutine can finish — and returns without waiting further. The
// goroutine may still be running when this returns; closing our own
// descriptor is the entire remedy for a lingering grandchild, so this never
// signals or kills any process, and it never waits longer than the grace.
func drainWithGrace(clk clock.Clock, done <-chan struct{}, onGraceExpired func()) {
	select {
	case <-done:
	case <-clk.After(ptyDrainGrace):
		onGraceExpired()
	}
}

// errRecordingWriter wraps sink and records the first error it returns, so
// startPTY's copy goroutine can report a sink write failure to buildResult
// without racing the goroutine that recorded it.
type errRecordingWriter struct {
	stream *ptyStream
	sink   io.Writer
}

func (w *errRecordingWriter) Write(p []byte) (int, error) {
	n, err := w.sink.Write(p)
	if err != nil {
		w.stream.mu.Lock()
		if w.stream.writeErr == nil {
			w.stream.writeErr = err
		}
		w.stream.mu.Unlock()
	}
	return n, err
}

// startPTY launches cmd on a pseudo-terminal and copies its output to sink.
//
// The child sees a terminal, so it line-buffers its output instead of
// switching to block buffering. Standard output and standard error merge.
//
// Unlike a plain pipe, cmd.Wait does not wait for the copy goroutine on its
// own: os/exec only tracks copy goroutines it started itself for cmd.Stdout
// and cmd.Stderr, and the pty path leaves both unset. Callers use the
// returned ptyStream's drain method to wait for it instead.
func startPTY(cmd *exec.Cmd, sink io.Writer) (*ptyStream, error) {
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 120})
	if err != nil {
		return nil, fmt.Errorf("supervisor: start on pty: %w", err)
	}

	stream := &ptyStream{master: ptmx, done: make(chan struct{})}
	recording := &errRecordingWriter{stream: stream, sink: sink}

	go func() {
		defer close(stream.done)
		defer func() { _ = ptmx.Close() }()
		// Reading the master after the child exits returns EIO on Linux and
		// a read error on Darwin, rather than a clean io.EOF. Either is the
		// normal end of the stream on the read side, not a failure, so that
		// error is discarded here. A write-side failure into sink is a real
		// failure; errRecordingWriter captures it into stream.writeErr,
		// which buildResult surfaces as Result.OutputErr.
		_, _ = io.Copy(recording, ptmx)
	}()
	return stream, nil
}
