// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/watch"
)

// PatternInput is one named pattern evaluated against every complete line.
type PatternInput struct {
	Name    string `json:"name"     jsonschema:"a short name you will read back in task_status"`
	Regex   string `json:"regex"    jsonschema:"a Go regular expression"`
	OnMatch string `json:"on_match" jsonschema:"record keeps a counter and the last match with its capture groups; kill ends the task; notify is recorded but wakes nobody, so use an until match condition to wake on output"`
}

// StartInput is task_start's input.
//
// PTY is a pointer so an omitted field means the default, which is true,
// rather than false. KillAfterS nil means no cap: the task runs until it
// ends on its own or a client signals it.
type StartInput struct {
	Command     string         `json:"command"                  jsonschema:"the program to run"`
	Args        []string       `json:"args,omitempty"           jsonschema:"arguments passed to the program"`
	Cwd         string         `json:"cwd,omitempty"            jsonschema:"working directory; defaults to the daemon's"`
	Name        string         `json:"name,omitempty"           jsonschema:"a name you will recognize later"`
	PTY         *bool          `json:"pty,omitempty"            jsonschema:"run under a pseudo-terminal; true by default, and required for task_write"`
	KillAfterS  *int           `json:"kill_after_s,omitempty"   jsonschema:"seconds after which the task is killed; omit for no cap, which is the default"`
	OnOutputCap string         `json:"on_output_cap,omitempty"  jsonschema:"rotate keeps the task running and bounds the log; this is the default"`
	Patterns    []PatternInput `json:"patterns,omitempty"       jsonschema:"patterns evaluated against every complete line of output"`
}

// params translates the agent-facing input into the daemon's parameters.
//
// The daemon's StartParams carries Harness, Session, and MaxOutput, which
// are plumbing an agent never sets. They stay at their zero values here.
func (in StartInput) params() daemon.StartParams {
	pats := make([]watch.Pattern, 0, len(in.Patterns))
	for _, p := range in.Patterns {
		pats = append(pats, watch.Pattern{
			Name:    p.Name,
			Regex:   p.Regex,
			OnMatch: watch.Action(p.OnMatch),
		})
	}
	return daemon.StartParams{
		Command:     in.Command,
		Args:        in.Args,
		Cwd:         in.Cwd,
		Name:        in.Name,
		PTY:         in.PTY,
		KillAfterS:  in.KillAfterS,
		OnOutputCap: in.OnOutputCap,
		Harness:     "",
		Session:     "",
		MaxOutput:   0,
		Patterns:    pats,
	}
}

func (s *server) addStart(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_start",
		Description: "Start a long-running command under the daemon and " +
			"return its id. Give it a name you will recognize, and patterns " +
			"for anything that should abort the run. Then call task_wait " +
			"rather than sleeping.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in StartInput,
	) (*mcp.CallToolResult, daemon.StartResult, error) {
		out, err := call[daemon.StartParams, daemon.StartResult](
			s.root, proto.VerbStart, in.params())
		return nil, out, err
	})
}
