package ledger

// The Index
//
// The journal keeps folded records in memory so the questions asked every few
// seconds (`jobs`, `runs NAME`, `trigger --wait`, a coalesced skip) never touch
// disk. It holds every record whose first line falls in the last 7 days
// (Window), and, for each harness, at least its newest 20 records (Tail) however
// old: a weekly job's `harness runs` answers from memory too. Older ranges are
// read from the day files through the same fold.
//
// The index knows what it does NOT hold. floor[h] is a seq below which records
// of harness h may be missing, and globalFloor the same for harnesses it has
// never seen. A query answered from memory is trusted only when those floors
// prove nothing older could have belonged in the answer; otherwise it reads the
// files. That is the difference between "this harness has 3 runs" and "this
// harness has 3 runs that I happen to remember".
//
// A record it trimmed can still receive lines: a skip record is one `decided`
// line, closed and so trimmable, while the supervisor coalesces firings into it
// for as long as the run in flight lasts, which can outlast the window. The
// index does not take it back. A committed `updated` or `closed` line for a key
// it does not hold, above floor 1, continues a record below the floor; folded
// alone it would be a stub with no outcome, filed at the line's own seq as the
// harness's newest run. The files hold the line, and Get, Pending and Query's
// file path fold it with the rest of its record. Pending reads the files for
// any record the index does not hold whole: one file read per firing, paid only
// by a skip older than the window and beyond the tail.
//
// The boot scan can meet a record without its first line the same way: an
// `updated` or `closed` line whose `opened` (or, for a skip, whose
// whole-carrying `decided`) lies in a file older than the scan reads. Folded
// alone it would be the same stub, so the scan sets such lines aside in cont
// instead of the index. Backfill folds a set-aside in once it loads the
// record's first line, adopts what is left once it has read everything there
// is (a first line nowhere to be found is REQ-2's genuine partial), and drops
// what stays beyond its reach; the window takes the rest. A `decided` line
// never waits in cont: it carries the whole record, and folds like an opening.
//
// Governing: SPEC-0022 REQ-2, REQ-7, REQ-15; design "The fold and the in-memory
// index"; SPEC-0014 REQ "Overlap Skip Coalescing".
//
// @joestump-agent 09/28/2026 - A committed line no longer turns a record the
// index trimmed into a Partial stub that Records and Query listed as the newest
// run, and Pending reads the files for a record the index does not hold whole
// rather than folding its queue over nothing.
//
// @joestump-agent 09/28/2026 - The boot scan no longer builds that stub either
// (harness#801): continuations of records whose first line is below the scan
// wait in cont, where Backfill completes, adopts or drops them, and the index
// never lists a run with no outcome as a harness's newest.

import (
	"cmp"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/stump-wtf/harness/internal/sealedlog"
)

type key struct {
	harness string
	id      int
}

type index struct {
	recs map[key]*Folded
	// by holds each harness's records ordered by Seq, oldest first.
	by map[string][]*Folded
	// floor[h]: every record of h with Seq >= floor[h] is here. Missing means
	// globalFloor.
	floor map[string]uint64
	// globalFloor: every record with Seq >= globalFloor is here. 1 means the
	// index holds the whole ledger.
	globalFloor uint64
	// windowStart: every record that started at or after it is here. Zero
	// means the whole ledger.
	windowStart time.Time
	// maxRunID is the highest run id seen per harness.
	maxRunID map[string]int
	// trimmedDay is the UTC day the index was last trimmed on.
	trimmedDay time.Time
	// cont holds the boot scan's set-aside continuations: folds of `updated`
	// and `closed` lines whose record's first line the scan never saw. See
	// the header.
	cont map[key]*Folded
	// scanAll is set while the boot scan reads every file there is, so a
	// continuation line whose record has no first line anywhere is a genuine
	// partial (REQ-2) and folds at once.
	scanAll bool
}

func newIndex() *index {
	return &index{
		recs:        map[key]*Folded{},
		by:          map[string][]*Folded{},
		floor:       map[string]uint64{},
		maxRunID:    map[string]int{},
		globalFloor: 1,
		cont:        map[key]*Folded{},
	}
}

// insert puts f in the harness's by list, ordered by Seq.
func (x *index) insert(f *Folded) {
	list := x.by[f.Harness]
	i, _ := slices.BinarySearchFunc(list, f.Seq, func(e *Folded, s uint64) int { return cmp.Compare(e.Seq, s) })
	x.by[f.Harness] = slices.Insert(list, i, f)
}

// bootApply folds one line of the boot scan in. It differs from apply the way
// commit differs for lines the writer has just written: an `updated` or
// `closed` line for a key the scan has not seen continues a record whose first
// line is in the files below the scan, and folding it alone would be a stub
// filed as the harness's newest run. It waits in cont, where Backfill
// completes, adopts or drops it. A `decided` line carries the whole record and
// folds like an opening.
func (x *index) bootApply(l Line) {
	k := key{l.Harness, l.RunID}
	if l.Type == TypeUpdated || l.Type == TypeClosed {
		if _, held := x.recs[k]; !held && !x.scanAll {
			c := x.cont[k]
			if c == nil {
				c = &Folded{}
				x.cont[k] = c
			}
			c.apply(l)
			if l.RunID > x.maxRunID[l.Harness] {
				x.maxRunID[l.Harness] = l.RunID
			}
			return
		}
	}
	if f := x.cont[k]; f != nil {
		// The record's first line met the scan after its continuation (a torn
		// tail can reorder lines): fold the two into the index.
		delete(x.cont, k)
		x.apply(l)
		if m := x.recs[k]; m != nil {
			overlay(m, f)
		}
		return
	}
	x.apply(l)
}

// apply folds one line in.
func (x *index) apply(l Line) {
	k := key{l.Harness, l.RunID}
	f := x.recs[k]
	if f == nil {
		f = &Folded{}
		x.recs[k] = f
		f.apply(l)
		x.insert(f)
	} else {
		before := f.Seq
		f.apply(l)
		if f.Seq != before {
			slices.SortFunc(x.by[l.Harness], func(a, b *Folded) int { return cmp.Compare(a.Seq, b.Seq) })
		}
	}
	if l.RunID > x.maxRunID[l.Harness] {
		x.maxRunID[l.Harness] = l.RunID
	}
}

// commit folds in a line the writer just committed. It differs from apply in
// one case: a line continuing a record (`updated`, `closed`) for a key the
// index does not hold, of a harness whose floor is above 1. That record's first
// line is below the floor, trimmed or never read, so the line is left to the
// files (see the header). A scan cannot do the same: backfill reads older files
// after newer ones, and completes a continuation it folded first.
//
// Loading the record from the files here instead would scan them in the
// writer's commit path, under the lock every Append waits on. A continuation
// whose first line the files have lost too (a pruned day file) folds there as
// REQ-2's partial record, which only the file readers then show.
func (x *index) commit(l Line) {
	if l.Type == TypeUpdated || l.Type == TypeClosed {
		if _, held := x.recs[key{l.Harness, l.RunID}]; !held && x.floorOf(l.Harness) > 1 {
			x.maxRunID[l.Harness] = max(x.maxRunID[l.Harness], l.RunID)
			return
		}
	}
	x.apply(l)
}

func (x *index) floorOf(h string) uint64 {
	if f, ok := x.floor[h]; ok {
		return f
	}
	return x.globalFloor
}

// trim drops closed records that are both outside the window and beyond each
// harness's tail, raising the floors past what it dropped. Open records are
// never dropped: reconciliation and the close path need them. A closed one can
// still receive lines (a coalesced skip); commit leaves those to the files.
func (x *index) trim(now time.Time, window time.Duration, tail int) {
	cutoff := now.Add(-window)
	for h, list := range x.by {
		keep := list[:0:0]
		var raised uint64
		for i, f := range list {
			newest := len(list) - i
			if newest > tail && f.Closed && f.when().Before(cutoff) {
				delete(x.recs, key{h, f.RunID})
				raised = max(raised, f.Seq+1)
				continue
			}
			keep = append(keep, f)
		}
		x.by[h] = keep
		if raised > x.floorOf(h) {
			x.floor[h] = raised
			x.globalFloor = max(x.globalFloor, raised)
		}
	}
	if x.windowStart.Before(cutoff) {
		x.windowStart = cutoff
	}
	for k, c := range x.cont {
		if c.when().Before(cutoff) {
			delete(x.cont, k)
		}
	}
}

// Query selects records (REQ-15).
type Query struct {
	// Names limits the harnesses; empty means every harness.
	Names []string
	// Since and Until bound the record's start (or, lacking one, its first
	// line). Zero means unbounded.
	Since, Until time.Time
	// Outcomes and Triggers keep only records with one of these values.
	Outcomes, Triggers []string
	// Limit caps the result; 0 means unbounded.
	Limit int
	// BeforeSeq keeps only records whose Seq is below it: the paging cursor.
	BeforeSeq uint64
}

func (q Query) match(f *Folded) bool {
	if len(q.Names) > 0 && !slices.Contains(q.Names, f.Harness) {
		return false
	}
	if q.BeforeSeq > 0 && f.Seq >= q.BeforeSeq {
		return false
	}
	w := f.when()
	if !q.Since.IsZero() && w.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && w.After(q.Until) {
		return false
	}
	if len(q.Outcomes) > 0 && !slices.Contains(q.Outcomes, f.Outcome) {
		return false
	}
	if len(q.Triggers) > 0 && !slices.Contains(q.Triggers, f.Trigger) {
		return false
	}
	return true
}

// fromMemory answers q from the index, and reports whether the answer is
// complete: whether the floors prove no record in the files could belong in it.
// Caller holds the ledger's lock.
func (x *index) fromMemory(q Query) ([]Folded, bool) {
	var out []Folded
	collect := func(list []*Folded) {
		for _, f := range list {
			if q.match(f) {
				out = append(out, *f)
			}
		}
	}
	thr := uint64(1)
	if len(q.Names) > 0 {
		for _, h := range q.Names {
			collect(x.by[h])
			thr = max(thr, x.floorOf(h))
		}
	} else {
		thr = x.globalFloor
		for h, list := range x.by {
			collect(list)
			thr = max(thr, x.floorOf(h))
		}
	}
	sortNewestFirst(out)
	switch {
	case thr <= 1:
		// The index holds everything there is.
	case !q.Since.IsZero() && !x.windowStart.IsZero() && !q.Since.Before(x.windowStart):
		// The range is inside the window, and the window is whole.
	case q.Limit > 0 && len(out) >= q.Limit && out[q.Limit-1].Seq >= thr:
		// The newest Limit matches are all above every floor, so nothing
		// below a floor could outrank them.
	default:
		return nil, false
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, true
}

func sortNewestFirst(out []Folded) {
	slices.SortFunc(out, func(a, b Folded) int { return cmp.Compare(b.Seq, a.Seq) })
}

// Query returns the records q selects, newest first, and the Seq of the oldest
// one returned (0 when none), which is the next page's BeforeSeq (REQ-15).
// It answers from memory when the index can prove its answer whole, and reads
// the day files otherwise. Either way it sees only committed lines.
func (l *Ledger) Query(q Query) ([]Folded, uint64, error) {
	l.mu.Lock()
	out, ok := l.idx.fromMemory(q)
	l.mu.Unlock()
	if !ok {
		var err error
		out, err = l.fromFiles(q)
		if err != nil {
			return nil, 0, err
		}
	}
	markPruned(out)
	var oldest uint64
	if len(out) > 0 {
		oldest = out[len(out)-1].Seq
	}
	return out, oldest, nil
}

// fromFiles folds every day file, overlays what the index holds (a line the
// writer committed after the scan passed its file), and applies q.
func (l *Ledger) fromFiles(q Query) ([]Folded, error) {
	l.mu.Lock()
	files := slices.Clone(l.files)
	l.mu.Unlock()
	want := func(h string) bool { return len(q.Names) == 0 || slices.Contains(q.Names, h) }
	recs := map[key]*Folded{}
	for _, df := range files {
		_, err := scanFile(filepath.Join(l.dir, df.name), func(ln Line) {
			if !want(ln.Harness) {
				return
			}
			k := key{ln.Harness, ln.RunID}
			f := recs[k]
			if f == nil {
				f = &Folded{}
				recs[k] = f
			}
			f.apply(ln)
		})
		if err != nil {
			return nil, errors.Join(ErrLedgerUnavailable, err)
		}
	}
	l.mu.Lock()
	for k, mem := range l.idx.recs {
		if !want(k.harness) {
			continue
		}
		f := recs[k]
		if f == nil {
			c := *mem
			recs[k] = &c
			continue
		}
		overlay(f, mem)
	}
	l.mu.Unlock()
	var out []Folded
	for _, f := range recs {
		if q.match(f) {
			out = append(out, *f)
		}
	}
	sortNewestFirst(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// overlay merges the index's copy of a record over the files' copy: the index
// has seen every line the files have for it since boot, and possibly more.
func overlay(f, mem *Folded) {
	if mem.LastSeq >= f.LastSeq {
		mergeOver(&f.Record, mem.Record)
		f.LastSeq = mem.LastSeq
	} else {
		mergeUnder(&f.Record, mem.Record)
	}
	f.HasOpened = f.HasOpened || mem.HasOpened
	f.Closed = f.Closed || mem.Closed
	if mem.Seq < f.Seq {
		f.Seq, f.FirstAt = mem.Seq, mem.FirstAt
	}
}

// markPruned sets LogPruned on records whose log has been deleted (REQ-12).
// Read, not written: keep_runs deletes the file, and the file's absence is the
// fact, so there is nothing for a line to get wrong. A log compressed once its
// run closed (<log>.zst; ADR-0007 as amended) is not pruned: only the absence
// of both forms is.
func markPruned(out []Folded) {
	for i := range out {
		if out[i].Log == "" {
			continue
		}
		if sealedlog.Missing(out[i].Log) {
			out[i].LogPruned = true
		}
	}
}

// Get returns one record.
func (l *Ledger) Get(harness string, id int) (Folded, bool, error) {
	l.mu.Lock()
	f, ok := l.idx.recs[key{harness, id}]
	var c Folded
	if ok {
		c = *f
	}
	l.mu.Unlock()
	if ok && !c.Partial() {
		out := []Folded{c}
		markPruned(out)
		return out[0], true, nil
	}
	recs, err := l.fromFiles(Query{Names: []string{harness}})
	if err != nil {
		return Folded{}, false, err
	}
	for _, r := range recs {
		if r.RunID == id {
			out := []Folded{r}
			markPruned(out)
			return out[0], true, nil
		}
	}
	if ok {
		return c, true, nil
	}
	return Folded{}, false, nil
}

// Pending returns one record as it will read once every queued line is
// written: the committed record with the queue folded over it. It is the
// writer's own read-your-writes, for a caller that computes its next line from
// its last (a coalesced skip counting firings), and never a view to publish:
// what it adds is not on disk yet.
//
// A record the index does not hold whole (trimmed, or Partial from the boot
// scan) is read from the files, as Get reads it, and the queue folded over
// that: folded over nothing, the queue alone is a record with no outcome,
// trigger or start.
func (l *Ledger) Pending(harness string, id int) (Folded, bool, error) {
	// One lock hold for the index and the queue: the writer moves a line from
	// one to the other under this lock, so reading them apart could see it in
	// neither and drop it.
	l.mu.Lock()
	var f Folded
	mem, ok := l.idx.recs[key{harness, id}]
	if ok {
		f = *mem
	}
	var queued []Line
	for _, p := range l.queue {
		if p.line.Harness == harness && p.line.RunID == id {
			queued = append(queued, p.line)
		}
	}
	l.mu.Unlock()
	if !ok || f.Partial() {
		// The queue was read first, so a line the writer commits in between
		// is in both, and folding a line twice changes nothing.
		var err error
		if f, ok, err = l.Get(harness, id); err != nil {
			return Folded{}, false, err
		}
	}
	for _, ln := range queued {
		f.apply(ln)
	}
	return f, ok || len(queued) > 0, nil
}

// Records returns every record of harness the index holds, oldest first: the
// last Window of it, and at least its newest Tail.
func (l *Ledger) Records(harness string) []Folded {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.idx.by[harness]
	out := make([]Folded, len(list))
	for i, f := range list {
		out[i] = *f
	}
	markPruned(out)
	return out
}

// OpenRecords returns every record the index holds that is opened and not
// closed, oldest first.
func (l *Ledger) OpenRecords() []Folded {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Folded
	for _, f := range l.idx.recs {
		if f.Open() {
			out = append(out, *f)
		}
	}
	slices.SortFunc(out, func(a, b Folded) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// MaxRunID returns the highest run id the index has seen for harness: a floor
// for the id allocator, so a run id can never be reissued while its record is
// still in the ledger.
func (l *Ledger) MaxRunID(harness string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.idx.maxRunID[harness]
}

// Backfill reads older day files, newest first, until every harness in names
// has at least Tail records in the index or the files run out. It runs once at
// boot, after Open and before reconciliation, so an open record older than the
// window (a resident up for a fortnight) is found and closed rather than left
// open forever. A record's first line it loads completes the boot scan's
// set-aside continuations of that record (cont); a harness whose older files
// run out without one adopts its set-asides whole, since a first line nowhere
// to be found is REQ-2's genuine partial; one that reaches its Tail with its
// set-asides unresolved leaves them to the window, the way it leaves records
// beyond its reach to the files.
func (l *Ledger) Backfill(names []string) error {
	l.mu.Lock()
	unread := l.files[:len(l.files)-l.scanned]
	unread = slices.Clone(unread)
	want := map[string]bool{}
	for _, h := range names {
		if len(l.idx.by[h]) < l.opts.Tail {
			want[h] = true
		}
	}
	l.mu.Unlock()
	var errs []error
	for i := len(unread) - 1; i >= 0 && len(want) > 0; i-- {
		fs, skipped, err := l.readInto(unread[i].name, want, l.idx.apply, nil)
		if err != nil {
			errs = append(errs, err)
		}
		l.mu.Lock()
		l.stats.Skipped += skipped
		for j := range l.files {
			if l.files[j].name == unread[i].name && l.files[j].firstSeq == 0 {
				l.files[j].firstSeq = fs
			}
		}
		for h := range want {
			if fs > 0 {
				l.idx.floor[h] = fs
			}
		}
		l.idx.resolveCont(want)
		for h := range want {
			if len(l.idx.by[h]) >= l.opts.Tail {
				delete(want, h)
			}
		}
		l.mu.Unlock()
	}
	l.mu.Lock()
	for h := range want {
		l.idx.floor[h] = 1 // read everything there is
	}
	l.idx.adoptCont(want)
	l.mu.Unlock()
	return errors.Join(errs...)
}

// resolveCont folds every set-aside continuation of a harness in want whose
// record the files have since delivered, and drops the ones of a harness that
// has since reached its Tail: their record stays beyond the reach backfill
// will ever have, and the index's floors already say so.
func (x *index) resolveCont(want map[string]bool) {
	for k, c := range x.cont {
		if !want[k.harness] {
			continue
		}
		if f := x.recs[k]; f != nil {
			overlay(f, c)
			delete(x.cont, k)
		}
	}
}

// adoptCont folds the set-aside continuations of harnesses whose whole ledger
// has now been read into the index: no first line was found because there is
// none, which makes the fold REQ-2's genuine partial record.
func (x *index) adoptCont(want map[string]bool) {
	for k, c := range x.cont {
		if !want[k.harness] {
			continue
		}
		delete(x.cont, k)
		x.recs[k] = c
		x.insert(c)
	}
}
