// SPDX-License-Identifier: AGPL-3.0-or-later

package watch_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rcarback/taskd/internal/clock"
	"github.com/rcarback/taskd/internal/watch"
)

func compile(t *testing.T, pats ...watch.Pattern) []*watch.CompiledPattern {
	t.Helper()
	got, err := watch.CompilePatterns(pats)
	if err != nil {
		t.Fatalf("CompilePatterns: %v", err)
	}
	return got
}

func TestCompilePatternsRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		pat  watch.Pattern
		want string
	}{
		"no name":      {watch.Pattern{Regex: "x", OnMatch: watch.ActionRecord}, "needs a name"},
		"no regex":     {watch.Pattern{Name: "a", OnMatch: watch.ActionRecord}, "needs a regex"},
		"bad regex":    {watch.Pattern{Name: "a", Regex: "([", OnMatch: watch.ActionRecord}, "cannot compile"},
		"bad action":   {watch.Pattern{Name: "a", Regex: "x", OnMatch: "explode"}, "unknown on_match"},
		"empty action": {watch.Pattern{Name: "a", Regex: "x"}, "unknown on_match"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := watch.CompilePatterns([]watch.Pattern{tc.pat})
			if err == nil {
				t.Fatalf("CompilePatterns(%+v) = nil, want an error", tc.pat)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCompilePatternsRejectsDuplicateNames(t *testing.T) {
	_, err := watch.CompilePatterns([]watch.Pattern{
		{Name: "err", Regex: "a", OnMatch: watch.ActionRecord},
		{Name: "err", Regex: "b", OnMatch: watch.ActionRecord},
	})
	if err == nil {
		t.Fatal("CompilePatterns accepted two patterns named err")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %q, want it to name the duplicate", err)
	}
}

func TestRecordKeepsCountAndLastMatch(t *testing.T) {
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{
		Name: "progress", Regex: `(\d+)/(\d+) done`, OnMatch: watch.ActionRecord,
	})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	in := "1/400 done\nnoise\n142/400 done\n"
	if _, err := tap.Write([]byte(in)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	stats := tap.Stats()
	if len(stats) != 1 {
		t.Fatalf("Stats() returned %d entries, want 1", len(stats))
	}
	s := stats[0]
	if s.Name != "progress" {
		t.Errorf("Name = %q, want %q", s.Name, "progress")
	}
	if s.Count != 2 {
		t.Errorf("Count = %d, want 2", s.Count)
	}
	if s.LastLine != "142/400 done" {
		t.Errorf("LastLine = %q, want the most recent hit", s.LastLine)
	}
	want := []string{"142", "400"}
	if len(s.Groups) != len(want) {
		t.Fatalf("Groups = %v, want %v", s.Groups, want)
	}
	for i := range want {
		if s.Groups[i] != want[i] {
			t.Errorf("Groups[%d] = %q, want %q", i, s.Groups[i], want[i])
		}
	}
}

func TestStatsGroupsAreNeverNil(t *testing.T) {
	// A nil slice marshals to JSON null. A client iterating groups would
	// have to test for that, so an unmatched pattern reports an empty
	// slice instead.
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{Name: "err", Regex: "error:", OnMatch: watch.ActionRecord})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	stats := tap.Stats()
	if stats[0].Groups == nil {
		t.Error("Groups is nil for an unmatched pattern, want an empty slice")
	}
	if stats[0].Count != 0 {
		t.Errorf("Count = %d, want 0", stats[0].Count)
	}
}

func TestStatsIsNeverNil(t *testing.T) {
	var sink bytes.Buffer
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), nil)
	if tap.Stats() == nil {
		t.Error("Stats() is nil for a task with no patterns, want an empty slice")
	}
}

func TestKillActionReportsOnce(t *testing.T) {
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{
		Name: "oom", Regex: "out of memory", OnMatch: watch.ActionKill,
	})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	if _, err := tap.Write([]byte("fine\nout of memory\nout of memory again\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case e := <-tap.Killed():
		if e.Name != "oom" {
			t.Errorf("Name = %q, want %q", e.Name, "oom")
		}
		if e.Line != "out of memory" {
			t.Errorf("Line = %q, want the first hit", e.Line)
		}
	default:
		t.Fatal("no kill event: a kill pattern matched and nothing reported it")
	}

	select {
	case e, ok := <-tap.Killed():
		if ok {
			t.Fatalf("second kill event %+v: the channel must close after the first", e)
		}
	default:
		t.Fatal("the kill channel is still open: a second match would block the observer")
	}
}

func TestKillPatternStillRecordsItsCount(t *testing.T) {
	// A kill pattern is also a counter. An agent reading status after the
	// kill needs to know what ended the task.
	var sink bytes.Buffer
	pats := compile(t, watch.Pattern{Name: "oom", Regex: "out of memory", OnMatch: watch.ActionKill})
	tap := watch.NewTap(&sink, clock.NewFake(time.Unix(0, 0)), pats)

	if _, err := tap.Write([]byte("out of memory\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.Stats()[0].Count; got != 1 {
		t.Errorf("Count = %d, want 1", got)
	}
}
