package ledger

// Trim Tests
//
// A skip record is a `decided` line, so it folds closed and the index may trim
// it while the supervisor still coalesces firings into it: the open-skip map
// keeps it for as long as the run in flight lasts, which can outlast the
// window. These tests trim one, then send it the `updated` line CoalesceRun
// writes (committed, queued, and read back after a restart) and check that no
// reader takes that line alone for the record: the index builds no stub from
// it, so it never lists a run with no outcome as the harness's newest, and
// Pending folds the queue over the record the files hold.
//
// Governing: SPEC-0022 REQ-2, REQ-15; design "The fold and the in-memory
// index"; SPEC-0014 REQ "Overlap Skip Coalescing".
//
// @joestump-agent 09/28/2026 - Added with the fix: before it, the first
// committed update of a trimmed skip made a Partial stub that Records and
// Query listed as the newest run, and that Pending handed CoalesceRun.

import (
	"io/fs"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// skipDecided is the `decided` line of a skip record, counting its first
// firing.
func skipDecided(h string, id int, at time.Time) Line {
	return Line{Type: TypeDecided, At: at, Harness: h, RunID: id, Record: Record{
		Kind: KindOneshot, Trigger: "schedule", Outcome: "skipped", Reason: "overlap",
		StartedAt: ptime(at), EndedAt: ptime(at), Coalesced: 1,
	}}
}

// coalescedTo is the `updated` line CoalesceRun writes: the absolute count.
func coalescedTo(h string, id, n int) Line {
	return Line{Type: TypeUpdated, Harness: h, RunID: id, Record: Record{Coalesced: n}}
}

func runIDs(recs []Folded) []int {
	var out []int
	for _, f := range recs {
		out = append(out, f.RunID)
	}
	return out
}

// wantWholeSkip fails unless f is run 1's skip record, first line and all,
// counting n firings.
func wantWholeSkip(t *testing.T, what string, f Folded, n int) {
	t.Helper()
	if f.RunID != 1 || f.Seq != 1 || f.Outcome != "skipped" || f.Trigger != "schedule" ||
		f.Reason != "overlap" || f.StartedAt == nil || f.Coalesced != n {
		t.Errorf("%s = run %d seq %d, outcome %q, trigger %q, reason %q, started %v, coalesced %d; "+
			"want run 1 seq 1 skipped/schedule/overlap with its start, coalesced %d",
			what, f.RunID, f.Seq, f.Outcome, f.Trigger, f.Reason, f.StartedAt, f.Coalesced, n)
	}
}

// trimmedSkip opens a ledger on a fake clock whose index has trimmed run 1, a
// skip record, and kept runs 4 and 5 (Window 1h, Tail 2).
func trimmedSkip(t *testing.T, dir string) (*Ledger, func() time.Time) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	opts := Options{Now: clock, Window: time.Hour, Tail: 2, RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond}
	l := openT(t, dir, opts)
	for id := 1; id <= 4; id++ {
		mustAppend(t, l, skipDecided("a", id, clock()), true)
	}
	// A synced Append returns once its line is on disk, which can be before
	// the writer commits it. Committed on the new day, run 4 would take the
	// trim below from run 5 and keep run 3.
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	mu.Lock()
	now = now.Add(48 * time.Hour)
	mu.Unlock()
	// The first commit of a new day trims runs 1-3: closed, older than the
	// window, and beyond the newest two.
	mustAppend(t, l, skipDecided("a", 5, clock()), true)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if got := runIDs(l.Records("a")); !slices.Equal(got, []int{4, 5}) {
		t.Fatalf("index holds runs %v before the update, want [4 5]: the trim this test needs did not happen", got)
	}
	return l, clock
}

// A committed update to a trimmed record stays in the files: the index lists
// no stub for it, and every reader that reaches below the floor folds it with
// the rest of its record.
func TestATrimmedRecordTakesNoStubFromAnUpdate(t *testing.T) {
	dir := t.TempDir()
	l, _ := trimmedSkip(t, dir)

	mustAppend(t, l, coalescedTo("a", 1, 2), false)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })

	if got := runIDs(l.Records("a")); !slices.Equal(got, []int{4, 5}) {
		t.Errorf("Records(a) = runs %v after the update, want [4 5]: a stub of run 1 came back", got)
	}
	// Answered from memory: the newest two are above the floor.
	recs, _, err := l.Query(Query{Names: []string{"a"}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := runIDs(recs); !slices.Equal(got, []int{5, 4}) {
		t.Errorf("Query(limit 2) = runs %v, want [5 4]: run 1 is the oldest record, not the newest", got)
	}
	// Answered from the files: run 1 is there, whole, with the update.
	recs, _, err = l.Query(Query{Names: []string{"a"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if got := runIDs(recs); !slices.Equal(got, []int{5, 4, 3, 2, 1}) {
		t.Fatalf("Query(limit 10) = runs %v, want [5 4 3 2 1]", got)
	}
	wantWholeSkip(t, "Query's run 1", recs[4], 2)

	f, ok, err := l.Pending("a", 1)
	if err != nil || !ok {
		t.Fatalf("Pending(a, 1) = ok %v, err %v", ok, err)
	}
	wantWholeSkip(t, "Pending(a, 1)", f, 2)
	f, ok, err = l.Get("a", 1)
	if err != nil || !ok {
		t.Fatalf("Get(a, 1) = ok %v, err %v", ok, err)
	}
	wantWholeSkip(t, "Get(a, 1)", f, 2)
}

// With the update still queued, Pending folds it over the record the files
// hold, not over nothing: CoalesceRun returns that record as the skip's
// decision.
func TestPendingFoldsTheQueueOverATrimmedRecord(t *testing.T) {
	dir := t.TempDir()
	l, _ := trimmedSkip(t, dir)

	var mu sync.Mutex
	failing := true
	realWrite := writeFile
	writeFile = func(f *os.File, b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return 0, fs.ErrPermission
		}
		return realWrite(f, b)
	}
	t.Cleanup(func() { writeFile = realWrite })

	mustAppend(t, l, coalescedTo("a", 1, 2), false)
	mustAppend(t, l, coalescedTo("a", 1, 3), false)
	if q := l.Stats().Queued; q != 2 {
		t.Fatalf("queued = %d, want both updates held behind the failing disk", q)
	}
	f, ok, err := l.Pending("a", 1)
	if err != nil || !ok {
		t.Fatalf("Pending(a, 1) = ok %v, err %v", ok, err)
	}
	wantWholeSkip(t, "Pending(a, 1) with both updates queued", f, 3)

	mu.Lock()
	failing = false
	mu.Unlock()
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if got := runIDs(l.Records("a")); !slices.Equal(got, []int{4, 5}) {
		t.Errorf("Records(a) = runs %v once the queue drains, want [4 5]", got)
	}
	f, _, _ = l.Pending("a", 1)
	wantWholeSkip(t, "Pending(a, 1) once the queue drains", f, 3)
}

// After a restart the boot scan reads only the window, so run 1 is in the
// index as a Partial record: its update without its decided line. Pending
// reads the files for it, as Get does.
func TestPendingReadsTheFilesForAPartialRecord(t *testing.T) {
	dir := t.TempDir()
	l, clock := trimmedSkip(t, dir)
	mustAppend(t, l, coalescedTo("a", 1, 2), true)
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	l = openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 2})
	var mem Folded
	for _, f := range l.Records("a") {
		if f.RunID == 1 {
			mem = f
		}
	}
	if !mem.Partial() {
		t.Fatalf("after the restart run 1 is %+v in the index, want the Partial fold of its update: "+
			"the case this test needs did not happen", mem)
	}
	f, ok, err := l.Pending("a", 1)
	if err != nil || !ok {
		t.Fatalf("Pending(a, 1) = ok %v, err %v", ok, err)
	}
	wantWholeSkip(t, "Pending(a, 1) after the restart", f, 2)
}
