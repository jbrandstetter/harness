package ledger

// Pending Tests
//
// Pending is the writer's read-your-writes: the Manager counts a coalesced skip
// by reading the record through it and appending the next count as a buffered
// `updated` line. These tests put that read-modify-write where a lost increment
// would have to come from: behind a writer that is held, so the lines really
// are queued, and on a record the index has trimmed, so the read cannot come
// from memory. The count stays exact in both. The history readers see only
// what is committed, and the first test pins that too: it is why a caller that
// reads the history after a burst waits for the queue to drain first.
//
// Governing: SPEC-0022 REQ-2, REQ-6; design "The fold and the in-memory index";
// SPEC-0014 REQ "Overlap Skip Coalescing".
//
// @joestump-agent 09/28/2026 - Added while chasing a flaky coalesced count in
// the supervisor's TestRunHistoryUnderConcurrency: the increments were queued,
// never lost, and these pin that.

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// skippedLine is the `decided` line of a skip record, counting its first firing.
func skippedLine(h string, id int, at time.Time) Line {
	return Line{Type: TypeDecided, At: at, Harness: h, RunID: id, Record: Record{
		Kind: KindOneshot, Trigger: "schedule", Outcome: "skipped", Reason: "overlap",
		StartedAt: ptime(at), EndedAt: ptime(at), Coalesced: 1,
	}}
}

// coalesce is Manager.CoalesceRun's read-modify-write on the ledger alone: one
// more firing, counted from Pending and appended buffered. It returns the count.
func coalesce(t *testing.T, l *Ledger, h string, id int) int {
	t.Helper()
	f, ok, err := l.Pending(h, id)
	if err != nil || !ok {
		t.Fatalf("Pending(%s, %d) = ok %v, err %v; want the record", h, id, ok, err)
	}
	n := max(f.Coalesced, 1) + 1
	if _, err := l.Append(Line{Type: TypeUpdated, Harness: h, RunID: id, Record: Record{Coalesced: n}}, false); err != nil {
		t.Fatal(err)
	}
	return n
}

// coalescedIn is the count Records reports for run id, or -1 when it holds no
// such record.
func coalescedIn(recs []Folded, id int) int {
	for _, f := range recs {
		if f.RunID == id {
			return f.Coalesced
		}
	}
	return -1
}

// updatedCounts is every count the day files hold in `updated` lines for run
// id, in file order: what is on disk, not what the index remembers.
func updatedCounts(t *testing.T, dir string, id int) []int {
	t.Helper()
	var out []int
	for _, ln := range fileLines(t, dir) {
		if ln["type"] == string(TypeUpdated) && ln["run_id"] == float64(id) {
			out = append(out, int(ln["coalesced"].(float64)))
		}
	}
	return out
}

// countsFrom is from, from+1, ..., to.
func countsFrom(from, to int) []int {
	var out []int
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

// TestPendingCountsEveryQueuedIncrement: with the writer held, every increment
// is still queued when the next one reads its base, and each reads the one
// before it. Records meanwhile reports only the committed count, and reaches
// the exact count once the queue drains.
func TestPendingCountsEveryQueuedIncrement(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	if _, err := l.Append(skippedLine("a", 1, time.Now()), true); err != nil {
		t.Fatal(err)
	}

	const firings = 50
	release := l.HoldWritesForTesting()
	t.Cleanup(release)
	for want := 2; want <= firings; want++ {
		if got := coalesce(t, l, "a", 1); got != want {
			t.Fatalf("firing %d counted %d: it read a base the queue had moved past", want, got)
		}
	}
	// The hold held: every increment is queued, none committed.
	if q := l.Stats().Queued; q != firings-1 {
		t.Fatalf("queued = %d, want all %d increments", q, firings-1)
	}
	if n := coalescedIn(l.Records("a"), 1); n != 1 {
		t.Errorf("Records counts %d with every increment queued, want the committed 1", n)
	}

	release()
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if n := coalescedIn(l.Records("a"), 1); n != firings {
		t.Errorf("Records counts %d once the queue drains, want %d", n, firings)
	}
	if got := updatedCounts(t, dir, 1); !slices.Equal(got, countsFrom(2, firings)) {
		t.Errorf("updated lines on disk = %v, want 2..%d, each once", got, firings)
	}
}

// TestPendingCountsARecordTheIndexTrimmed: a skip record older than the window
// and beyond the tail leaves the index, and its count keeps going exactly
// anyway: while its increments are queued (Pending reads the files, then the
// queue) and once they are committed.
func TestPendingCountsARecordTheIndexTrimmed(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	dir := t.TempDir()
	l := openT(t, dir, Options{Now: clock, Window: time.Hour, Tail: 2})

	if _, err := l.Append(skippedLine("a", 1, clock()), true); err != nil {
		t.Fatal(err)
	}
	for id := 2; id <= 4; id++ {
		if _, err := l.Append(skippedLine("a", id, clock()), true); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	now = now.Add(48 * time.Hour)
	mu.Unlock()
	// The first commit of a new day trims the index.
	if _, err := l.Append(skippedLine("a", 5, clock()), true); err != nil {
		t.Fatal(err)
	}
	if n := coalescedIn(l.Records("a"), 1); n != -1 {
		t.Fatalf("run 1 is still in the index (count %d); the trim this test needs did not happen", n)
	}

	release := l.HoldWritesForTesting()
	t.Cleanup(release)
	want := 1
	for range 3 {
		want++
		if got := coalesce(t, l, "a", 1); got != want {
			t.Fatalf("queued firing %d on a trimmed record counted %d", want, got)
		}
	}
	release()
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	for range 3 {
		want++
		if got := coalesce(t, l, "a", 1); got != want {
			t.Fatalf("committed firing %d on a trimmed record counted %d", want, got)
		}
		waitFor(t, func() bool { return l.Stats().Queued == 0 })
	}

	if f, ok, err := l.Get("a", 1); err != nil || !ok || f.Coalesced != want {
		t.Errorf("Get(a, 1) = %d (ok %v, err %v), want %d", f.Coalesced, ok, err, want)
	}
	if got := updatedCounts(t, dir, 1); !slices.Equal(got, countsFrom(2, want)) {
		t.Errorf("updated lines on disk = %v, want 2..%d, each once", got, want)
	}
}

// TestHoldWritesForTestingQueuesUntilRelease: the hold keeps a buffered line
// out of the file and out of the readers, and the release lets it through.
func TestHoldWritesForTestingQueuesUntilRelease(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	release := l.HoldWritesForTesting()
	t.Cleanup(release)
	if _, err := l.Append(skippedLine("a", 1, time.Now()), false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(fileLines(t, dir)); n != 0 {
		t.Fatalf("%d lines on disk while held, want 0", n)
	}
	if recs := l.Records("a"); len(recs) != 0 {
		t.Fatalf("Records sees %d records while held, want 0", len(recs))
	}
	release()
	waitFor(t, func() bool { return l.Stats().Queued == 0 })
	if n := len(fileLines(t, dir)); n != 1 {
		t.Errorf("%d lines on disk after the release, want 1", n)
	}
}
