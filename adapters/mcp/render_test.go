// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpadapter

import (
	"slices"
	"testing"

	"github.com/rcarback/taskd/internal/watch"
)

// TestRenderUntilRoundTripsEachSupportedKind checks that rendering a
// parsed condition set back to the command-line spelling and re-parsing it
// yields the same conditions, for every kind watch.ParseUntil can produce
// and for a set combining several of them.
func TestRenderUntilRoundTripsEachSupportedKind(t *testing.T) {
	for _, until := range []string{
		"exit",
		"idle:300",
		"elapsed:120",
		"lines:50",
		"exit,idle:300,elapsed:120,lines:50",
	} {
		conds, err := watch.ParseUntil(until)
		if err != nil {
			t.Fatalf("ParseUntil(%q): %v", until, err)
		}

		rendered := renderUntil(conds)

		reparsed, err := watch.ParseUntil(rendered)
		if err != nil {
			t.Fatalf("ParseUntil(renderUntil(%q)) = %q: %v", until, rendered, err)
		}

		if !slices.Equal(reparsed, conds) {
			t.Errorf("ParseUntil(%q) = %+v, renderUntil -> %q, re-parsed = %+v; want the round trip to hold",
				until, conds, rendered, reparsed)
		}
	}
}

// TestRenderUntilOfNoConditionsIsEmpty checks the case addWait relies on to
// omit --until from the emitted command when the caller supplied none.
func TestRenderUntilOfNoConditionsIsEmpty(t *testing.T) {
	if got := renderUntil(nil); got != "" {
		t.Errorf("renderUntil(nil) = %q, want empty", got)
	}
}
