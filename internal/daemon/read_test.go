// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/rcarback/taskd/internal/proto"
)

// TestRewind covers every branch of the free function directly: no test in
// this file otherwise exercises the branches below "if pending == 0", since
// the only escape-producing case in the suite, TestReadStripsEscapeSequences,
// ends its buffer with a newline and never leaves anything pending.
func TestRewind(t *testing.T) {
	tests := []struct {
		name         string
		next, cursor int64
		pending      int
		live         bool
		want         int64
	}{
		{
			name: "no pending bytes returns next unchanged",
			next: 10, cursor: 5, pending: 0, live: true,
			want: 10,
		},
		{
			name: "a partial rewind that still advances returns the rewound cursor",
			next: 10, cursor: 2, pending: 3, live: true,
			want: 7,
		},
		{
			name: "every byte pending on a live task waits: returns the caller's cursor",
			next: 10, cursor: 8, pending: 5, live: true,
			want: 8,
		},
		{
			name: "every byte pending on a finished task drops the sequence: returns next",
			next: 10, cursor: 8, pending: 5, live: false,
			want: 10,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewind(tc.next, tc.cursor, tc.pending, tc.live); got != tc.want {
				t.Fatalf("rewind(%d, %d, %d, %v) = %d, want %d",
					tc.next, tc.cursor, tc.pending, tc.live, got, tc.want)
			}
		})
	}
}

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

	// Assert equality, not containment: containment alone would also pass a
	// cursor that repeats bytes across the two reads, which is exactly the
	// failure this test's name promises to catch. The task runs under a PTY
	// (the default), whose line discipline translates each outgoing \n to
	// \r\n, so that is what the log actually holds.
	const want = "alpha\r\nbeta\r\n"
	if joined := a.Data + b.Data; joined != want {
		t.Fatalf("the two reads joined to %q, want exactly %q", joined, want)
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

// TestReadReachesTheDropAndFinishBranchAfterAnUnfinishedEscape covers the
// two branches rewind's table test cannot: a real read boundary that cuts an
// escape sequence, and the daemon-level state (a finished task) that decides
// whether the cut bytes are held or dropped.
//
// The script writes "text\033[", an unfinished CSI sequence with no final
// byte. A single read from cursor 0 cannot reach Written in one call: the
// leading "text" is real progress, so rewind advances the cursor past it
// (to 4) rather than discarding it, leaving the pending "\033[" unconsumed.
// A second read starting at that cursor reads only the pending tail: every
// byte in that chunk is pending, the task has already finished, so rewind
// drops the sequence and returns Next = Written with EOF true.
func TestReadReachesTheDropAndFinishBranchAfterAnUnfinishedEscape(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "printf 'text\\033['")

	first, err := callVerb(t, d, "task_read", ReadParams{ID: id, MaxBytes: 4})
	if err != nil {
		t.Fatalf("first task_read: %v", err)
	}
	a := first.(ReadResult)
	if a.Data != "text" || a.EOF {
		t.Fatalf("first read = %+v, want Data=%q EOF=false", a, "text")
	}

	second, err := callVerb(t, d, "task_read", ReadParams{ID: id, Since: &a.Next, MaxBytes: 1024})
	if err != nil {
		t.Fatalf("second task_read: %v", err)
	}
	b := second.(ReadResult)
	e, _ := d.Reg.Get(id)
	written := e.Record().Written

	if b.Data != "" {
		t.Fatalf("second read Data = %q, want empty: the unfinished escape carries no text", b.Data)
	}
	if b.Next != written {
		t.Fatalf("second read Next = %d, want Written = %d", b.Next, written)
	}
	if !b.EOF {
		t.Fatal("second read EOF = false, want true: the unfinished sequence must not make eof unreachable")
	}
}

func TestReadReachesALiveTaskThroughTheOpenStore(t *testing.T) {
	d := newDaemon(t)

	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", "cat " + fifo},
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)

	// Opening a FIFO for reading blocks until a writer connects, and nothing
	// has opened it for writing yet, so the task is deterministically still
	// running here — no polling or sleep needed to establish that.
	if !e.Live() {
		t.Fatal("Live() = false immediately after task_start, want true: the task is blocked reading the FIFO")
	}

	got2, err := callVerb(t, d, "task_read", ReadParams{ID: id})
	if err != nil {
		t.Fatalf("task_read on a live task: %v", err)
	}
	res := got2.(ReadResult)
	if res.EOF {
		t.Fatal("EOF = true for a task still running, want false: this must go through Entry.Log's live-store branch")
	}

	// Unblock the task: opening the FIFO for writing and closing it without
	// writing anything hands cat an EOF, so it exits and the entry reaches a
	// terminal state that waitForState can observe.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0) //nolint:gosec // fifo is a path this test just created under t.TempDir(), not untrusted input
	if err != nil {
		t.Fatalf("open fifo for writing: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close fifo writer: %v", err)
	}
	waitForState(t, e)
}

// request encodes v as one verb request.
func request(t *testing.T, verb proto.Verb, v any) proto.Request {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return proto.Request{Verb: verb, Params: b}
}

// TestReadOverTheSocketStaysUnderTheMessageCap goes through the real socket
// rather than calling the handler, because the defect only appears at the
// protocol boundary: WriteMessage refuses a response over MaxMessageBytes,
// and an unclamped read builds one whenever the task produced more than a
// mebibyte. The client's symptom was "proto: decode: EOF", which names
// neither the cap nor the verb.
//
// The tail path was the worse half: store.Tail is bounded only by the task's
// retained log, which defaults to 8 MiB.
func TestReadOverTheSocketStaysUnderTheMessageCap(t *testing.T) {
	d, sock := start(t)
	d.Register()

	noPTY := false
	started := roundTrip(t, sock, request(t, proto.VerbStart, StartParams{
		Command: "sh", Args: []string{"-c", `head -c 2000000 /dev/zero | tr '\0' 'a'`},
		PTY: &noPTY,
	}))
	if !started.OK {
		t.Fatalf("task_start: %s", started.Error)
	}
	var sr StartResult
	if err := json.Unmarshal(started.Result, &sr); err != nil {
		t.Fatalf("decode StartResult: %v", err)
	}
	e, _ := d.Reg.Get(sr.ID)
	waitForState(t, e)
	if w := e.Record().Written; w < 2_000_000 {
		t.Fatalf("Written = %d, want the full 2 MB the task produced", w)
	}

	huge := 1 << 20
	for _, tc := range []struct {
		name   string
		params ReadParams
	}{
		{"tail over the whole retained log", ReadParams{ID: sr.ID, Tail: &huge}},
		{"since with an oversized max_bytes", ReadParams{ID: sr.ID, MaxBytes: 4 << 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := roundTrip(t, sock, request(t, proto.VerbRead, tc.params))
			if !res.OK {
				t.Fatalf("task_read: %s", res.Error)
			}
			var rr ReadResult
			if err := json.Unmarshal(res.Result, &rr); err != nil {
				t.Fatalf("decode ReadResult: %v", err)
			}
			if len(rr.Data) > maxResultBytes {
				t.Fatalf("Data is %d bytes, over the %d byte clamp", len(rr.Data), maxResultBytes)
			}
			if len(rr.Data) == 0 {
				t.Fatal("Data is empty: the clamp must return the bytes it can carry, not none")
			}
		})
	}
}

// TestReadResumesPastTheClampOnTheSinceBranch proves the clamp costs the
// caller nothing on the cursor path: the bytes it held back are still there
// on the next call, and eof stays false until they have all been read.
func TestReadResumesPastTheClampOnTheSinceBranch(t *testing.T) {
	d := newDaemon(t)
	noPTY := false
	got, err := callVerb(t, d, "task_start", StartParams{
		Command: "sh", Args: []string{"-c", `head -c 2000000 /dev/zero | tr '\0' 'a'`},
		PTY: &noPTY,
	})
	if err != nil {
		t.Fatalf("task_start: %v", err)
	}
	id := got.(StartResult).ID
	e, _ := d.Reg.Get(id)
	waitForState(t, e)

	total := 0
	cursor := int64(0)
	for range 64 {
		res, err := callVerb(t, d, "task_read", ReadParams{ID: id, Since: &cursor, MaxBytes: 4 << 20})
		if err != nil {
			t.Fatalf("task_read: %v", err)
		}
		rr := res.(ReadResult)
		total += len(rr.Data)
		cursor = rr.Next
		if rr.EOF {
			break
		}
	}
	if want := int(e.Record().Written); total != want {
		t.Fatalf("read %d bytes across the clamped calls, want all %d", total, want)
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

func TestSearchMatchEncodesEmptyBeforeAndAfterAsArraysNotMissing(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "echo error")

	// Context defaults to 0, so Before and After are both empty for this
	// match: the case that would otherwise encode as a missing key rather
	// than [].
	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "error"})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 1 {
		t.Fatalf("found %d matches, want 1", len(res.Matches))
	}

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"before":[]`) {
		t.Fatalf("encoded = %s, want \"before\":[] rather than a missing key", b)
	}
	if !strings.Contains(string(b), `"after":[]`) {
		t.Fatalf("encoded = %s, want \"after\":[] rather than a missing key", b)
	}
	// The wire name for Match.LineNumber, asserted by name: a Go doc comment
	// is invisible to a client, and renaming the tag must fail a test.
	if !strings.Contains(string(b), `"retained_line_number"`) {
		t.Fatalf("encoded = %s, want the retained_line_number key", b)
	}
}

// TestSearchOnEmptyOutputReportsNoPhantomLine guards against
// strings.Split("", "\n") producing a one-element slice holding the empty
// string: without a guard, a task with no output at all would present one
// phantom empty line to the matcher, and a permissive pattern like "^$"
// would report a match that does not exist.
func TestSearchOnEmptyOutputReportsNoPhantomLine(t *testing.T) {
	d := newDaemon(t)
	id := startAndWait(t, d, "exit 0")

	got, err := callVerb(t, d, "task_search", SearchParams{ID: id, Regex: "^$"})
	if err != nil {
		t.Fatalf("task_search: %v", err)
	}
	res := got.(SearchResult)
	if len(res.Matches) != 0 {
		t.Fatalf("Matches = %+v, want none: the task produced no output at all", res.Matches)
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
