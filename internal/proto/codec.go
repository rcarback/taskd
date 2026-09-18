// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

import (
	"encoding/json"
	"fmt"
	"io"
)

// MaxMessageBytes caps one encoded message in either direction. A client is
// a local process the user already controls, so this is a guard against a
// runaway encode rather than against an attacker.
const MaxMessageBytes = 1 << 20

// WriteMessage encodes v as JSON followed by a newline.
//
// The newline is not needed to find the message boundary, since a connection
// carries exactly one message in each direction. It is there so an operator
// can read a captured socket stream.
//
// An oversize message is rejected before anything is written, so a failed
// call never leaves a partial message on the connection.
func WriteMessage(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("proto: encode: %w", err)
	}
	if len(b) > MaxMessageBytes {
		return fmt.Errorf("proto: message is %d bytes, over the %d byte limit", len(b), MaxMessageBytes)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("proto: write: %w", err)
	}
	return nil
}

// ReadMessage decodes one JSON message from r into v.
//
// It reads at most MaxMessageBytes, so a peer that never stops writing
// cannot exhaust memory here.
func ReadMessage(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, MaxMessageBytes))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("proto: decode: %w", err)
	}
	return nil
}
