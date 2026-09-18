// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

func startAndWait(t *testing.T, d *Daemon, script string) string {
	t.Helper()
	got, err := callVerb(t, d, "task_start", StartParams{Command: "sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)
	waitForState(t, e)
	return id
}

func TestReadReturnsTheLogFromACursor(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'alpha\\nbeta\\n'")

	got, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	res := got.(ReadResult)
	if !strings.Contains(res.Data, "alpha") || !strings.Contains(res.Data, "beta") {
		t.Fatalf("Data = %q, want both lines", res.Data)
	}
	if res.Next == 0 {
		t.Fatal("Next = 0 after reading output")
	}
	if !res.EOF {
		t.Fatal("EOF = false for a finished task read to the end")
	}
}

func TestReadFromACursorReturnsEachByteOnce(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'alpha\\nbeta\\n'")

	first, err := callVerb(t, d, "task_read", ReadParams{ID: id, MaxBytes: 4})
	if err != nil {
		t.Fatalf("first task_read: %v", err)
	}
	a := first.(ReadResult)

	second, err := callVerb(t, d, "task_read", ReadParams{ID: id, Since: &a.Next})
	if err != nil {
		t.Fatalf("second task_read: %v", err)
	}
	b := second.(ReadResult)

	if joined := a.Data + b.Data; !strings.Contains(joined, "alpha") || !strings.Contains(joined, "beta") {
		t.Fatalf("the two reads joined to %q, want the whole log exactly once", joined)
	}
}

func TestReadStripsEscapeSequences(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf '\\033[31mred\\033[0m\\n'")

	got, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	res := got.(ReadResult)
	if strings.Contains(res.Data, "\x1b") {
		t.Fatalf("Data = %q, want no escape bytes", res.Data)
	}
	if !strings.Contains(res.Data, "red") {
		t.Fatalf("Data = %q, want the text", res.Data)
	}
}

func TestReadTailReturnsTheLastLines(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "for i in 1 2 3 4 5; do echo line$i; done")

	two := 2
	got, err := callVerb(t, d, "task_read", ReadParams{ID: id, Tail: &two})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	res := got.(ReadResult)
	if strings.Contains(res.Data, "line3") {
		t.Fatalf("Data = %q, want only the last two lines", res.Data)
	}
	if !strings.Contains(res.Data, "line5") {
		t.Fatalf("Data = %q, want the last line", res.Data)
	}
}

func TestReadReportsTruncatedBytesAfterRotation(t *testing.T) {
	d := newDaemon(t)
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "for i in $(seq 1 500); do echo padding-line-$i; done"},
		MaxOutput: 1024,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)
	waitForState(t, e)

	res, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read: %v", err)
	}
	if res.(ReadResult).TruncatedBytes == 0 {
		t.Fatal("TruncatedBytes = 0 after rotation discarded output")
	}
}

func TestReadRejectsBothSinceAndTail(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "echo hi")

	zero, two := int64(0), 2
	if _, err := callVerb(t, d, "task_read", ReadParams{ID: id, Since: &zero, Tail: &two}); err == nil {
		t.Fatal("task_read accepted both since and tail")
	}
}

func TestSearchFindsMatchesWithContext(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'one\\ntwo\\nerror: boom\\nfour\\nfive\\n'")

	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "error:", Context: 1})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 1 {
		t.Fatalf("found %d matches, want 1", len(res.Matches))
	}
	m := res.Matches[0]
	if !strings.Contains(m.Line, "boom") {
		t.Fatalf("Line = %q, want the matching line", m.Line)
	}
	if len(m.Before) != 1 || !strings.Contains(m.Before[0], "two") {
		t.Fatalf("Before = %v, want the preceding line", m.Before)
	}
	if len(m.After) != 1 || !strings.Contains(m.After[0], "four") {
		t.Fatalf("After = %v, want the following line", m.After)
	}
}

func TestSearchRejectsABadRegex(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "echo hi")

	if _, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "("}); err == nil {
		t.Fatal("task_search accepted an invalid regular expression")
	}
}

func TestSearchWithNoMatchesEncodesAnEmptyArrayNotNull(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "echo hi")

	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "no-such-pattern"})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 0 {
		t.Fatalf("Matches = %+v, want empty", res.Matches)
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"matches":[]`) {
		t.Fatalf("encoded = %s, want \"matches\":[] rather than null", b)
	}
}

func TestSearchCapsTheMatchCount(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "for i in 1 2 3 4 5; do echo error; done")

	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "error", MaxMatches: 2})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 2 {
		t.Fatalf("found %d matches, want the cap of 2", len(res.Matches))
	}
	if !res.More {
		t.Fatal("More = false, want true when the cap hid matches")
	}
}
