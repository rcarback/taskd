// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ansi removes terminal escape sequences from captured output.
package ansi

import "regexp"

// escape matches, in order: CSI sequences, OSC sequences ended by BEL or by
// the string terminator, and two-byte escape sequences. The pattern operates
// on bytes below 0x80 only, so multi-byte UTF-8 text passes through unchanged.
//
// The two-byte class deliberately excludes both 0x5B and 0x5D. Those bytes
// open a CSI and an OSC sequence, so neither ever ends a two-byte escape.
var escape = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` +
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
		`|\x1b[@-Z\\^_]`,
)

// incomplete matches a sequence that starts in the buffer and is not
// finished by the end of it: a lone escape, a CSI without its final byte, or
// an OSC without its terminator. Every alternative is anchored to the end,
// so a match can only be the buffer's trailing bytes.
var incomplete = regexp.MustCompile(
	`\x1b$` +
		`|\x1b\[[0-9;?]*[ -/]*$` +
		`|\x1b\][^\x07\x1b]*\x1b?$`,
)

// Strip removes terminal escape sequences from b.
//
// clean is a fresh copy of b without the sequences it could classify. It
// never aliases b.
//
// pendingLen counts trailing bytes of b that begin a sequence b does not
// finish. Those bytes are not in clean. A caller reading a stream in chunks
// must rewind its cursor by pendingLen so the sequence arrives whole on the
// next read. A caller holding the whole stream, such as a tail or a search,
// discards them: they are an unfinished sequence, not output.
func Strip(b []byte) (clean []byte, pendingLen int) {
	if loc := incomplete.FindIndex(b); loc != nil {
		pendingLen = len(b) - loc[0]
		b = b[:loc[0]]
	}
	return escape.ReplaceAll(b, nil), pendingLen
}
