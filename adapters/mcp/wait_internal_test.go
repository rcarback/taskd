// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import "testing"

// TestBackgroundInstructionRejectsAnIDWithAShellMetacharacter exercises
// idPattern directly against backgroundInstruction.
//
// In normal operation resolveIDs resolves every id before it reaches this
// function, so only a real id the daemon issued ever arrives here. This
// test bypasses that and hands backgroundInstruction the malicious id
// directly, the way a future caller of this function might if resolveIDs
// were ever skipped or a new path forgot to call it. idPattern is the
// defence that only runs when nothing has gone wrong upstream, and this
// keeps it under direct test even though the current tool handler never
// exercises it with unresolved input.
func TestBackgroundInstructionRejectsAnIDWithAShellMetacharacter(t *testing.T) {
	if _, err := backgroundInstruction("/root", []string{"$(touch /tmp/PWNED)"}, ""); err == nil {
		t.Fatal("backgroundInstruction accepted an id containing a shell metacharacter")
	}
}

// TestBackgroundInstructionAcceptsARealID pins the accepting side of the
// same check, so a mutation that widens idPattern to reject everything
// cannot hide behind the rejection test above.
func TestBackgroundInstructionAcceptsARealID(t *testing.T) {
	instr, err := backgroundInstruction("/root", []string{"87e-v2"}, "")
	if err != nil {
		t.Fatalf("backgroundInstruction rejected a well-formed id: %v", err)
	}
	if instr == "" {
		t.Fatal("backgroundInstruction returned no instruction for a well-formed id")
	}
}
