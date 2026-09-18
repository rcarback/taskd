// SPDX-License-Identifier: AGPL-3.0-or-later

package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	want := Request{Verb: VerbStatus, Params: json.RawMessage(`{"ids":["a"]}`)}
	if err := WriteMessage(&buf, want); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	var got Request
	if err := ReadMessage(&buf, &got); err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if got.Verb != want.Verb {
		t.Fatalf("Verb = %q, want %q", got.Verb, want.Verb)
	}
	if string(got.Params) != string(want.Params) {
		t.Fatalf("Params = %s, want %s", got.Params, want.Params)
	}
}

func TestWriteMessageEndsWithANewline(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, Response{OK: true}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	if b := buf.Bytes(); len(b) == 0 || b[len(b)-1] != '\n' {
		t.Fatalf("encoded message does not end with a newline: %q", b)
	}
}

func TestWriteMessageRejectsAnOversizeMessage(t *testing.T) {
	var buf bytes.Buffer
	huge := Response{Error: strings.Repeat("x", MaxMessageBytes+1)}
	if err := WriteMessage(&buf, huge); err == nil {
		t.Fatal("WriteMessage accepted a message over MaxMessageBytes, want an error")
	}
	if buf.Len() != 0 {
		t.Fatalf("WriteMessage wrote %d bytes for a rejected message, want 0", buf.Len())
	}
}

func TestReadMessageRejectsAnOversizeMessage(t *testing.T) {
	line := append(bytes.Repeat([]byte("x"), MaxMessageBytes+1), '\n')
	var got Request
	if err := ReadMessage(bytes.NewReader(line), &got); err == nil {
		t.Fatal("ReadMessage accepted a stream over MaxMessageBytes, want an error")
	}
}

func TestReadMessageRejectsTruncatedJSON(t *testing.T) {
	var got Request
	if err := ReadMessage(strings.NewReader(`{"verb":"task_st`), &got); err == nil {
		t.Fatal("ReadMessage accepted truncated JSON, want an error")
	}
}
