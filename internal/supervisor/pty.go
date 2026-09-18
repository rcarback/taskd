// SPDX-License-Identifier: AGPL-3.0-or-later

package supervisor

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/creack/pty"
)

// startPTY launches cmd on a pseudo-terminal and copies its output to sink.
//
// The child sees a terminal, so it line-buffers its output instead of
// switching to block buffering. Standard output and standard error merge.
//
// The returned channel closes once the copy goroutine has drained the pty
// master, so a caller can wait for all output to land in sink before
// finalizing a result. Unlike a plain pipe, cmd.Wait does not wait for this
// goroutine on its own: os/exec only tracks copy goroutines it started
// itself for cmd.Stdout and cmd.Stderr, and the pty path leaves both unset.
func startPTY(cmd *exec.Cmd, sink io.Writer) (<-chan struct{}, error) {
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 120})
	if err != nil {
		return nil, fmt.Errorf("supervisor: start on pty: %w", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = ptmx.Close() }()
		// Reading the master after the child exits returns EIO on Linux and
		// a read error on Darwin, rather than a clean io.EOF. Either is the
		// normal end of the stream, not a failure, so the copy error is
		// discarded here: it must never reach Result.OutputErr.
		_, _ = io.Copy(sink, ptmx)
	}()
	return done, nil
}
