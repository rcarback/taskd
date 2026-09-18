// SPDX-License-Identifier: AGPL-3.0-or-later

package ansi

import "testing"

func TestStrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "hello world", "hello world"},
		{"colour codes", "\x1b[31merror:\x1b[0m boom", "error: boom"},
		{"cursor movement", "a\x1b[2Kb", "ab"},
		{"osc title with bell", "\x1b]0;my title\x07done", "done"},
		{"osc title with string terminator", "\x1b]0;t\x1b\\done", "done"},
		{"multibyte text survives", "café ✓ \U0001F600", "café ✓ \U0001F600"},
		{"multibyte next to escapes", "\x1b[32m✓\x1b[0m ok", "✓ ok"},
		{"newlines preserved", "one\ntwo\n", "one\ntwo\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := Strip([]byte(tc.in))
			if string(got) != tc.want {
				t.Fatalf("Strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripDoesNotAliasInput(t *testing.T) {
	in := []byte("\x1b[31mred\x1b[0m")
	out, _ := Strip(in)
	out[0] = 'X'
	if in[5] == 'X' {
		t.Fatal("Strip returned a slice aliasing its input")
	}
}

func TestStripReportsAnIncompleteTail(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantClean   string
		wantPending int
	}{
		{"a lone trailing escape", "ok\x1b", "ok", 1},
		{"a truncated CSI", "ok\x1b[31", "ok", 4},
		{"a truncated OSC", "ok\x1b]0;title", "ok", 9},
		{"a truncated OSC on its terminator", "ok\x1b]0;t\x1b", "ok", 6},
		{"a complete OSC is not pending", "ok\x1b]0;t\x07done", "okdone", 0},
		{"a complete CSI is not pending", "ok\x1b[31mdone", "okdone", 0},
		{"no escapes at all", "plain text", "plain text", 0},
		{"an escape mid-buffer is not pending", "a\x1bXb", "ab", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clean, pending := Strip([]byte(c.in))
			if string(clean) != c.wantClean {
				t.Fatalf("clean = %q, want %q", clean, c.wantClean)
			}
			if pending != c.wantPending {
				t.Fatalf("pendingLen = %d, want %d", pending, c.wantPending)
			}
		})
	}
}

func TestStripRejoinsASequenceSplitAcrossChunks(t *testing.T) {
	// The whole stream is "red\x1b[31mtext", cut inside the CSI sequence.
	// A reader that rewinds by pendingLen sees the sequence whole on the
	// next read and never emits its bytes as literal output.
	whole := "red\x1b[31mtext"
	cut := 6 // "red\x1b[3"

	first, pending := Strip([]byte(whole[:cut]))
	if string(first) != "red" {
		t.Fatalf("first chunk clean = %q, want %q", first, "red")
	}

	resume := cut - pending
	second, pendingTwo := Strip([]byte(whole[resume:]))
	if string(second) != "text" {
		t.Fatalf("second chunk clean = %q, want %q", second, "text")
	}
	if pendingTwo != 0 {
		t.Fatalf("second chunk pendingLen = %d, want 0", pendingTwo)
	}
	if got := string(first) + string(second); got != "redtext" {
		t.Fatalf("rejoined = %q, want %q", got, "redtext")
	}
}

func TestStripDoesNotTreatABareCloseBracketAsATwoByteEscape(t *testing.T) {
	// ESC ] starts an OSC sequence. It is never a complete two-byte escape.
	// Before this fix the payload "0;title" was emitted as literal text.
	clean, pending := Strip([]byte("\x1b]0;title"))
	if len(clean) != 0 {
		t.Fatalf("clean = %q, want empty: the whole buffer is an unfinished OSC", clean)
	}
	if pending != 9 {
		t.Fatalf("pendingLen = %d, want 9", pending)
	}
}
