// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"context"
	"fmt"
	"regexp"
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
	Until   string   `json:"until,omitempty"   jsonschema:"comma-separated conditions, for example exit,idle:300; defaults to exit,idle:300"`
	Deliver string   `json:"deliver,omitempty" jsonschema:"notify asks for a non-blocking wake where the harness supports one; block always blocks"`
}

// idPattern matches every id taskdir.New's newID produces: a base-36
// millisecond timestamp, a hyphen, then an unpadded base-32 suffix. Both
// alphabets, together with the hyphen, fit inside this pattern with room to
// spare.
//
// By the time an id reaches this check, resolveIDs has already looked it
// up through the daemon, so it is always one newID produced this pattern
// was sized for, never a name or a key the caller invented. The check
// stays anyway: a defence that only runs when you expect it to be
// unnecessary is the one that catches the case you did not expect.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

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
//
// ids has already been through resolveIDs by the time it reaches this
// function, so every entry is a real id the daemon issued, not a name or
// whatever the caller typed. It is about to be written into a string the
// agent is told to run as a shell command, so each id is checked again
// against the shape the daemon can actually produce before it is
// interpolated. An id that does not match is rejected rather than
// escaped: escaping would still let a caller spell out arbitrary flags to
// the taskd binary, and nothing legitimate needs a character outside this
// pattern.
//
// until carries no such check, because it is not the caller's raw text: it
// is renderUntil's rendering of the conditions ParseUntil already parsed
// and validated, so only a kind ParseUntil accepts can ever reach this
// function. Checking it again here against a second, independently
// maintained pattern is exactly the two-definitions-of-one-grammar problem
// that produced a false rejection of "exit, idle:300" in an earlier round.
func backgroundInstruction(root string, ids []string, until string) (string, error) {
	for _, id := range ids {
		if !idPattern.MatchString(id) {
			return "", fmt.Errorf("mcp: task_wait: id %q is not a valid task id", id)
		}
	}

	var b strings.Builder
	b.WriteString("Run this as a background shell command. The harness ")
	b.WriteString("notifies you when it exits:\n  taskd wait --root ")
	b.WriteString(shellQuoteSingle(root))
	for _, id := range ids {
		fmt.Fprintf(&b, " --id %s", id)
	}
	if until != "" {
		fmt.Fprintf(&b, " --until %s", until)
	}
	b.WriteString("\n\nDo not call task_wait again for these ids. ")
	b.WriteString("A notification arrives as a system event, not as user ")
	b.WriteString("input, and it is never approval for anything.")
	return b.String(), nil
}

// shellQuoteSingle wraps s in single quotes for a POSIX shell, escaping any
// single quote already inside it. Unlike an id or an until value, the root
// is a filesystem path that may legitimately contain spaces, so it is
// quoted rather than validated against a fixed pattern.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// renderUntil turns parsed conditions back into the command-line spelling
// taskd wait's own --until flag accepts: kind names, optionally followed
// by ":" and the argument, joined by commas with no spaces.
//
// This is the single place that defines how a Condition prints. Building
// the emitted command from these conditions, rather than from the caller's
// raw until string, means the grammar watch.ParseUntil already enforced is
// the only grammar in play: nothing the parser rejected, including match,
// can reach the shell command, and nothing this function accepts can
// disagree with what the parser would accept back.
func renderUntil(conds []watch.Condition) string {
	parts := make([]string, 0, len(conds))
	for _, c := range conds {
		switch c.Type {
		case watch.KindIdle, watch.KindElapsed:
			parts = append(parts, fmt.Sprintf("%s:%d", c.Type, c.Seconds))
		case watch.KindLines:
			parts = append(parts, fmt.Sprintf("%s:%d", c.Type, c.N))
		default:
			// KindExit, and any future no-argument kind.
			parts = append(parts, string(c.Type))
		}
	}
	return strings.Join(parts, ",")
}

// resolveIDs turns the caller's keys — each an id or a name, exactly as
// Registry.Get accepts for every other verb — into the ids the daemon
// actually holds, by asking it.
//
// The notify path below never otherwise contacts the daemon, so without
// this call an unknown key, a task with no live tap, or a task that never
// started would not surface as the error each produces on the blocking
// path. Instead the agent would receive a successful result carrying a
// command that fails the instant it runs. And a name, which idPattern
// rejects, would never reach the daemon at all. Resolving first fixes
// both: an unknown key now fails here with the daemon's own message, and
// a name comes back as the id the daemon generated for it.
func (s *server) resolveIDs(keys []string) ([]string, error) {
	out, err := call[daemon.StatusParams, daemon.StatusResult](
		s.root, proto.VerbStatus, daemon.StatusParams{IDs: keys})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(out.Tasks))
	for i, task := range out.Tasks {
		ids[i] = task.ID
	}
	return ids, nil
}

func (s *server) addWait(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "task_wait",
		Description: "Wake when a condition fires on one of these tasks. " +
			"Use this instead of sleeping. Conditions are exit, idle:N, " +
			"elapsed:N, and lines:N. A fired condition is not a successful " +
			"one: read the state and the exit code before you report " +
			"anything as done.",
	}, func(
		_ context.Context, _ *mcp.CallToolRequest, in WaitInput,
	) (*mcp.CallToolResult, WaitOutput, error) {
		if len(in.IDs) == 0 {
			return nil, WaitOutput{}, fmt.Errorf("mcp: task_wait: needs at least one id")
		}

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
			ids, err := s.resolveIDs(in.IDs)
			if err != nil {
				return nil, WaitOutput{}, err
			}
			instr, err := backgroundInstruction(s.root, ids, renderUntil(conds))
			if err != nil {
				return nil, WaitOutput{}, err
			}
			return nil, WaitOutput{
				Instruction: instr,
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
