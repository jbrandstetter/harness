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
//
// @joestump-agent 09/28/2026 - With harness#801 the boot scan sets the
// update aside instead of stubbing the index, so the reopen test asserts the
// absence and three more cover the scan's set-aside: Backfill completes one,
// adopts one whose first line is nowhere, and a ledger with no older files
// folds one at once as REQ-2's partial.

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

// After a restart the boot scan reads only the window, and run 1's update
// waits in the scan's set-aside instead of the index: Records lists no stub
// for it, and Pending reads the files for a run the index does not hold, as
// Get does.
func TestPendingReadsTheFilesForAPartialRecord(t *testing.T) {
	dir := t.TempDir()
	l, clock := trimmedSkip(t, dir)
	mustAppend(t, l, coalescedTo("a", 1, 2), true)
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	l = openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 2})
	// Only day three is inside the window, so run 5 is all the scan holds
	// before backfill; run 1's update is in the set-aside, not a stub.
	if got := runIDs(l.Records("a")); !slices.Equal(got, []int{5}) {
		t.Fatalf("Records(a) = runs %v after the restart, want [5]: run 1's update built a stub in the index (harness#801)", got)
	}
	f, ok, err := l.Pending("a", 1)
	if err != nil || !ok {
		t.Fatalf("Pending(a, 1) = ok %v, err %v", ok, err)
	}
	wantWholeSkip(t, "Pending(a, 1) after the restart", f, 2)
	f, ok, err = l.Get("a", 1)
	if err != nil || !ok {
		t.Fatalf("Get(a, 1) = ok %v, err %v", ok, err)
	}
	wantWholeSkip(t, "Get(a, 1) after the restart", f, 2)
}

// Backfill loads a set-aside continuation's record: the trimmed skip lists
// whole, at its decided line's seq, with the coalesced count the update
// carried.
func TestBackfillCompletesASetAsideContinuation(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	l := openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 3})
	mustAppend(t, l, skipDecided("a", 1, clock()), true)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	mu.Lock()
	now = now.Add(48 * time.Hour)
	mu.Unlock()
	mustAppend(t, l, skipDecided("a", 2, clock()), true)
	mustAppend(t, l, coalescedTo("a", 1, 2), true)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	l = openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 3})
	if got := runIDs(l.Records("a")); !slices.Equal(got, []int{2}) {
		t.Fatalf("Records(a) = runs %v before backfill, want [2]: run 1's update built a stub (harness#801)", got)
	}
	if err := l.Backfill([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	recs := l.Records("a")
	if got := runIDs(recs); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("Records(a) = runs %v after backfill, want [1 2]", got)
	}
	wantWholeSkip(t, "backfilled run 1", recs[0], 2)
	if recs[0].Partial() || recs[0].Seq != 1 {
		t.Errorf("backfilled run 1 = seq %d partial %v, want whole at seq 1", recs[0].Seq, recs[0].Partial())
	}
}

// A set-aside whose record's first line is in no file the ledger has is
// REQ-2's genuine partial: backfill reads everything there is, adopts it, and
// it lists alongside the records it found.
func TestBackfillAdoptsAContinuationWithNoFirstLine(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	l := openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 5})
	mustAppend(t, l, opened("a", 1, clock()), true)
	mustAppend(t, l, closed("a", 1, clock(), "success"), true)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	mu.Lock()
	now = now.Add(48 * time.Hour)
	mu.Unlock()
	mustAppend(t, l, coalescedTo("a", 2, 1), true)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	l = openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 5})
	if recs := l.Records("a"); len(recs) != 0 {
		t.Fatalf("Records(a) = %+v before backfill, want nothing: run 2's orphan update listed itself", recs)
	}
	if err := l.Backfill([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	recs := l.Records("a")
	if got := runIDs(recs); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("Records(a) = runs %v after backfill, want [1 2]", got)
	}
	if recs[0].Outcome != "success" || !recs[0].HasOpened {
		t.Errorf("run 1 = %+v, want the whole success record", recs[0])
	}
	if !recs[1].Partial() {
		t.Errorf("run 2 = %+v, want the partial its lost opening makes (REQ-2)", recs[1])
	}
}

// A ledger whose every file the boot scan reads has no older place for a
// first line to hide: a continuation folds at once as REQ-2's partial, the
// way it always has.
func TestBootScanWithoutOlderFilesFoldsAContinuationAtOnce(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	l := openT(t, dir, Options{Now: func() time.Time { return now }})
	mustAppend(t, l, opened("a", 1, now), true)
	mustAppend(t, l, coalescedTo("a", 2, 1), true)
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if err := l.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}

	l2 := openT(t, dir, Options{Now: func() time.Time { return now }})
	recs := l2.Records("a")
	if got := runIDs(recs); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("Records(a) = runs %v, want [1 2]", got)
	}
	if recs[0].Outcome != "running" || !recs[0].HasOpened {
		t.Errorf("run 1 = %+v, want its opened fold", recs[0])
	}
	if !recs[1].Partial() {
		t.Errorf("run 2 = %+v, want the partial its missing opening makes (REQ-2)", recs[1])
	}
}
