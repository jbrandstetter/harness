package ledger

import (
	"os"
	"sync"
	"testing"
)

// SkipSyncForTesting makes every Ledger in this test binary write its lines
// without fdatasync. The ordering does not change: a line still commits, and
// becomes visible to Records, Get and Query, only once the writer has written
// it and its (now instant) sync has returned.
//
// No test can observe what fdatasync adds: that is only visible after a power
// loss, and a killed process leaves the page cache behind. What tests did
// observe was the runner's disk. Every run lifecycle the supervisor, daemon
// and cmd/harness tests wait on is read back from the ledger, and so waited on
// an fsync; on a shared CI runner that disk is contended, and run 13777 logged
// syncs past the 2s SyncTimeout and shutdown drains still queued 5s later,
// failing tests on budgets the code under test never used. This package's own
// ordering, query and import tests failed the same way, with a synced Append
// reporting ErrLedgerUnavailable. Its sync handling (a failing, a stalled
// sync) is tested by installing a syncFileFn that fails or stalls, which this
// does not prevent.
//
// Call it from TestMain, before any Ledger opens. It panics outside a test
// binary, so it cannot turn a daemon's durability off.
func SkipSyncForTesting() {
	if !testing.Testing() {
		panic("ledger: SkipSyncForTesting called outside a test binary")
	}
	syncFileFn = func(*os.File) error { return nil }
}

// HoldWritesForTesting stops l's writer before its next write and returns the
// release. Lines appended meanwhile stay queued, as they do behind a disk that
// has fallen behind: Pending folds them in, and Records, Get and Query do not
// see them until the release lets the writer commit them. A synced Append waits
// as it would on a slow disk, up to SyncTimeout, so hold only around buffered
// ones. Release before closing l, or Close waits out its timeout.
//
// It panics outside a test binary.
func (l *Ledger) HoldWritesForTesting() (release func()) {
	if !testing.Testing() {
		panic("ledger: HoldWritesForTesting called outside a test binary")
	}
	h := make(chan struct{})
	l.hold.Store(&h)
	return sync.OnceFunc(func() {
		l.hold.Store(nil)
		close(h)
	})
}
