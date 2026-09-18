// SPDX-License-Identifier: AGPL-3.0-or-later

// Package proto defines the request and response messages that a taskd
// client and the daemon exchange over a Unix socket.
package proto

import "encoding/json"

// Verb names one operation the daemon performs.
type Verb string

// The verbs this plan implements. Plan 3 adds task_wait.
const (
	VerbStart  Verb = "task_start"
	VerbStatus Verb = "task_status"
	VerbRead   Verb = "task_read"
	VerbSearch Verb = "task_search"
	VerbSignal Verb = "task_signal"
	VerbWrite  Verb = "task_write"
)

// Request is the single message a client sends on a connection.
//
// Params carries the verb's own arguments, left as raw JSON so this package
// does not depend on every verb's parameter type.
type Request struct {
	Verb   Verb            `json:"verb"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the single message the daemon sends back.
//
// OK reports whether the verb succeeded. Exactly one of Result and Error
// carries content: Result on success, Error on failure.
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}
