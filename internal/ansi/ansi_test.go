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
			if got := string(Strip([]byte(tc.in))); got != tc.want {
				t.Fatalf("Strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripDoesNotAliasInput(t *testing.T) {
	in := []byte("\x1b[31mred\x1b[0m")
	out := Strip(in)
	out[0] = 'X'
	if in[5] == 'X' {
		t.Fatal("Strip returned a slice aliasing its input")
	}
}
