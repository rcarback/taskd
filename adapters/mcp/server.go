// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcpadapter exposes the taskd verbs as Model Context Protocol
// tools over standard input and output.
//
// The package name differs from its directory because the SDK's own package
// is named mcp, and two packages with one name would force an alias in
// every file that imports both.
package mcpadapter

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/version"
)

// Harness names the agent runtime that launched this server.
//
// A server cannot work this out for itself. Codex passes an MCP server nine
// environment variables and none of them names the session, the thread, or
// the working directory, so the value arrives as a flag from the
// configuration that launched the server.
type Harness string

// The harnesses this adapter recognizes.
const (
	// HarnessGeneric blocks on every wait. It is the default.
	HarnessGeneric Harness = "generic"

	// HarnessClaudeCode returns a background-command instruction for a
	// wait that asks for notify delivery.
	HarnessClaudeCode Harness = "claude-code"

	// HarnessCodex behaves as HarnessGeneric. Codex delivery is unverified
	// and this adapter ships none.
	HarnessCodex Harness = "codex"
)

// ParseHarness converts a flag value into a Harness.
func ParseHarness(s string) (Harness, error) {
	switch h := Harness(s); h {
	case HarnessGeneric, HarnessClaudeCode, HarnessCodex:
		return h, nil
	default:
		return "", fmt.Errorf(
			"mcp: unknown harness %q: want generic, claude-code, or codex", s)
	}
}

// server holds what every tool handler needs.
type server struct {
	root    string
	harness Harness
}

// New builds an MCP server that serves the taskd tools from one root.
func New(root string, h Harness) *mcp.Server {
	s := &server{root: root, harness: h}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "taskd", Version: version.String()}, nil)
	s.addStatus(srv)
	s.addStart(srv)
	s.addRead(srv)
	s.addSearch(srv)
	s.addSignal(srv)
	s.addWrite(srv)
	return srv
}

// Run serves the tools on the standard streams until the client
// disconnects.
func Run(ctx context.Context, root string, h Harness) error {
	if err := New(root, h).Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcp: serve: %w", err)
	}
	return nil
}

// StatusInput is task_status's input. With no ids it lists every task.
type StatusInput struct {
	IDs []string `json:"ids,omitempty" jsonschema:"task ids to report; omit to list every task"`
	All bool     `json:"all,omitempty"  jsonschema:"include tasks that have already ended"`
}

// addStatus registers task_status.
func (s *server) addStatus(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_status",
		Description: "Terse state for one or more tasks. With no ids, list " +
			"every task. Read the state before you report a task as done: " +
			"a fired condition is not a successful one.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in StatusInput,
	) (*mcp.CallToolResult, daemon.StatusResult, error) {
		out, err := call[daemon.StatusParams, daemon.StatusResult](
			s.root, proto.VerbStatus, daemon.StatusParams{
				IDs:     in.IDs,
				Session: "",
				All:     in.All,
			})
		return nil, out, err
	})
}
