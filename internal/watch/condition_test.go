// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"strings"
	"testing"

	"github.com/rcarback/taskd/internal/watch"
)

func TestDefaultsAreExitAndIdle(t *testing.T) {
	got := watch.Defaults()
	want := []watch.Condition{
		{Type: watch.KindExit},
		{Type: watch.KindIdle, Seconds: 300},
	}
	if len(got) != len(want) {
		t.Fatalf("Defaults() returned %d conditions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Defaults()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDefaultsCarryNoAbsoluteComponent(t *testing.T) {
	// The spec is explicit: silence is the safety net, elapsed time is not.
	// An elapsed default would wake every agent on a slow-but-healthy build.
	for _, c := range watch.Defaults() {
		if c.Type == watch.KindElapsed {
			t.Fatalf("Defaults() includes %v: the default must have no absolute component", c)
		}
	}
}

func TestValidateRejectsBadConditions(t *testing.T) {
	cases := map[string]struct {
		cond watch.Condition
		want string
	}{
		"unknown type":      {watch.Condition{Type: "forever"}, "unknown condition type"},
		"idle without time": {watch.Condition{Type: watch.KindIdle}, "needs a positive seconds"},
		"negative elapsed":  {watch.Condition{Type: watch.KindElapsed, Seconds: -1}, "needs a positive seconds"},
		"match without re":  {watch.Condition{Type: watch.KindMatch}, "needs a pattern"},
		"bad regex":         {watch.Condition{Type: watch.KindMatch, Pattern: "(["}, "cannot compile"},
		"lines without n":   {watch.Condition{Type: watch.KindLines}, "needs a positive n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := watch.Validate([]watch.Condition{tc.cond})
			if err == nil {
				t.Fatalf("Validate(%+v) = nil, want an error", tc.cond)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsEveryKind(t *testing.T) {
	conds := []watch.Condition{
		{Type: watch.KindExit},
		{Type: watch.KindIdle, Seconds: 300},
		{Type: watch.KindElapsed, Seconds: 600},
		{Type: watch.KindMatch, Pattern: "error|panic", Name: "err"},
		{Type: watch.KindLines, N: 500},
	}
	if err := watch.Validate(conds); err != nil {
		t.Fatalf("Validate(every kind) = %v, want nil", err)
	}
}

func TestParseUntil(t *testing.T) {
	got, err := watch.ParseUntil("exit,idle:300,lines:50")
	if err != nil {
		t.Fatalf("ParseUntil: %v", err)
	}
	want := []watch.Condition{
		{Type: watch.KindExit},
		{Type: watch.KindIdle, Seconds: 300},
		{Type: watch.KindLines, N: 50},
	}
	if len(got) != len(want) {
		t.Fatalf("ParseUntil returned %d conditions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ParseUntil()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseUntilEmptyGivesDefaults(t *testing.T) {
	got, err := watch.ParseUntil("")
	if err != nil {
		t.Fatalf("ParseUntil(\"\"): %v", err)
	}
	if len(got) != 2 || got[0].Type != watch.KindExit || got[1].Type != watch.KindIdle {
		t.Fatalf("ParseUntil(\"\") = %+v, want the defaults", got)
	}
}
