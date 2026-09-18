// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ansi removes terminal escape sequences from captured output.
package ansi

import "regexp"

// escape matches, in order: CSI sequences, OSC sequences ended by BEL or by
// the string terminator, and two-byte escape sequences. The pattern operates
// on bytes below 0x80 only, so multi-byte UTF-8 text passes through unchanged.
var escape = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` +
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
		`|\x1b[@-Z\\-_]`,
)

// Strip returns a copy of b with terminal escape sequences removed.
func Strip(b []byte) []byte {
	return escape.ReplaceAll(b, nil)
}
