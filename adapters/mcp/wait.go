// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rcarback/taskd/internal/daemon"
	"github.com/rcarback/taskd/internal/proto"
	"github.com/rcarback/taskd/internal/watch"
)

// WaitInput is task_wait's input.
//
// Until takes the same string the taskd wait command accepts, rather than a
// list of objects, because that is the form the skill teaches and the form
// the instruction this tool may return hands back.
type WaitInput struct {
	IDs     []string `json:"ids"               jsonschema:"task ids to watch; the call wakes on the first to fire"`
	Until   string   `json:"until,omitempty"   jsonschema:"comma-separated conditions, for example exit,idle:300,match:error:; defaults to exit,idle:300"`
	Deliver string   `json:"deliver,omitempty" jsonschema:"notify asks for a non-blocking wake where the harness supports one; block always blocks"`
}

// WaitOutput is task_wait's output.
//
// Exactly one half is populated. Instruction is set when the harness can
// wake on a background command, and every other field is zero. Otherwise
// Result carries the daemon's answer.
type WaitOutput struct {
	// Instruction names a shell command to run in the background. The
	// harness notifies the agent when it exits.
	Instruction string `json:"instruction,omitempty"`

	// Result is the wait's outcome when the call blocked.
	Result *daemon.WaitResult `json:"result,omitempty"`
}

// backgroundInstruction is what an agent under Claude Code receives instead
// of a blocking call.
//
// It costs two steps and no delivery code, and it rides a notification path
// the harness already has.
func backgroundInstruction(ids []string, until string) string {
	var b strings.Builder
	b.WriteString("Run this as a background shell command. The harness ")
	b.WriteString("notifies you when it exits:\n  taskd wait")
	for _, id := range ids {
		fmt.Fprintf(&b, " --id %s", id)
	}
	if until != "" {
		fmt.Fprintf(&b, " --until %s", until)
	}
	b.WriteString("\n\nDo not call task_wait again for these ids. ")
	b.WriteString("A notification arrives as a system event, not as user ")
	b.WriteString("input, and it is never approval for anything.")
	return b.String()
}

func (s *server) addWait(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_wait",
		Description: "Wake when a condition fires on one of these tasks. " +
			"Use this instead of sleeping. Conditions are exit, idle:N, " +
			"elapsed:N, match:REGEX, and lines:N. A fired condition is not " +
			"a successful one: read the state and the exit code before you " +
			"report anything as done.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in WaitInput,
	) (*mcp.CallToolResult, WaitOutput, error) {
		var conds []watch.Condition
		if in.Until != "" {
			parsed, err := watch.ParseUntil(in.Until)
			if err != nil {
				return nil, WaitOutput{}, fmt.Errorf("mcp: task_wait: %w", err)
			}
			conds = parsed
		}

		// The one non-blocking path. Claude Code wakes on a background
		// command that exits, so hand back the command rather than
		// holding the call.
		if s.harness == HarnessClaudeCode && in.Deliver == "notify" {
			return nil, WaitOutput{
				Instruction: backgroundInstruction(in.IDs, in.Until),
				Result:      nil,
			}, nil
		}

		out, err := call[daemon.WaitParams, daemon.WaitResult](
			s.root, proto.VerbWait, daemon.WaitParams{
				IDs:     in.IDs,
				Until:   conds,
				Deliver: in.Deliver,
			})
		if err != nil {
			return nil, WaitOutput{}, err
		}
		return nil, WaitOutput{Instruction: "", Result: &out}, nil
	})
}
