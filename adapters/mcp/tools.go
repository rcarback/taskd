// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
)

// ReadInput is task_read's input. Since and Tail are mutually exclusive.
type ReadInput struct {
	ID       string `json:"id"                  jsonschema:"the task id"`
	Since    *int64 `json:"since,omitempty"     jsonschema:"cursor from a previous read; returns only newer output; mutually exclusive with tail"`
	Tail     *int   `json:"tail,omitempty"      jsonschema:"return the last N lines instead of reading from a cursor; mutually exclusive with since"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"cap the bytes this call returns"`
}

// SearchInput is task_search's input.
type SearchInput struct {
	ID         string `json:"id"                    jsonschema:"the task id"`
	Regex      string `json:"regex"                 jsonschema:"a Go regular expression to match against each line"`
	Context    int    `json:"context,omitempty"     jsonschema:"lines of context to return on each side of a match"`
	MaxMatches int    `json:"max_matches,omitempty" jsonschema:"stop after this many matches"`
}

// SignalInput is task_signal's input.
type SignalInput struct {
	ID     string `json:"id"                jsonschema:"the task id"`
	Signal string `json:"signal,omitempty"  jsonschema:"TERM or KILL; TERM is the default and escalates to KILL after the grace period"`
	GraceS *int   `json:"grace_s,omitempty" jsonschema:"seconds to wait after TERM before sending KILL"`
}

// WriteInput is task_write's input.
type WriteInput struct {
	ID   string `json:"id"   jsonschema:"the task id"`
	Data string `json:"data" jsonschema:"bytes to write to the task's standard input"`
}

func (s *server) addRead(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_read",
		Description: "Read a task's output by cursor, or the last N lines. " +
			"Pass since with the cursor from your previous read to get only " +
			"new output. Pass tail for a post-mortem snapshot instead. " +
			"since and tail are mutually exclusive; the daemon rejects a " +
			"call that sets both. Check truncated_bytes on every response: " +
			"a truncated log is how you conclude that a failed build " +
			"succeeded.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in ReadInput,
	) (*mcp.CallToolResult, daemon.ReadResult, error) {
		out, err := call[daemon.ReadParams, daemon.ReadResult](
			s.root, proto.VerbRead, daemon.ReadParams{
				ID:       in.ID,
				Since:    in.Since,
				Tail:     in.Tail,
				MaxBytes: in.MaxBytes,
			})
		return nil, out, err
	})
}

func (s *server) addSearch(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_search",
		Description: "Search a task's log with a regular expression and " +
			"return matches with context lines.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in SearchInput,
	) (*mcp.CallToolResult, daemon.SearchResult, error) {
		out, err := call[daemon.SearchParams, daemon.SearchResult](
			s.root, proto.VerbSearch, daemon.SearchParams{
				ID:         in.ID,
				Regex:      in.Regex,
				Context:    in.Context,
				MaxMatches: in.MaxMatches,
			})
		return nil, out, err
	})
}

func (s *server) addSignal(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_signal",
		Description: "Send TERM to a task, then KILL after a grace period. " +
			"Pass signal: \"KILL\" to send KILL directly, with no grace period.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in SignalInput,
	) (*mcp.CallToolResult, daemon.SignalResult, error) {
		out, err := call[daemon.SignalParams, daemon.SignalResult](
			s.root, proto.VerbSignal, daemon.SignalParams{
				ID:     in.ID,
				Signal: in.Signal,
				GraceS: in.GraceS,
			})
		return nil, out, err
	})
}

func (s *server) addWrite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_write",
		Description: "Write to a task's standard input. The task needs a " +
			"pseudo-terminal, which is the default. A task started with " +
			"pty false has no input channel.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in WriteInput,
	) (*mcp.CallToolResult, daemon.WriteResult, error) {
		out, err := call[daemon.WriteParams, daemon.WriteResult](
			s.root, proto.VerbWrite, daemon.WriteParams{
				ID:   in.ID,
				Data: in.Data,
			})
		return nil, out, err
	})
}
