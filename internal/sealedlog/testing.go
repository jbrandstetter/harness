package sealedlog

import (
	"os"
	"testing"
)

// SkipSyncForTesting makes every compression in this test binary skip its two
// fsyncs, the temp file's and the directory's. The order of the steps does
// not change: the temp is still written, renamed into place, and only then is
// the plain file removed.
//
// It exists for the same reason as ledger.SkipSyncForTesting: what an fsync
// adds is only observable after a power cut, and what tests did observe was a
// shared CI runner whose fsync takes seconds under load. A test that waits for
// a compression should not also wait on the runner's disk.
//
// Call it from TestMain. It panics outside a test binary, so it cannot turn a
// daemon's durability off.
func SkipSyncForTesting() {
	if !testing.Testing() {
		panic("sealedlog: SkipSyncForTesting called outside a test binary")
	}
	syncFile = func(*os.File) error { return nil }
	syncDir = func(string) error { return nil }
}
