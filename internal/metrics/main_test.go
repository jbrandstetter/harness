package metrics

import (
	"os"
	"testing"

	"github.com/stump-wtf/harness/internal/ledger"
)

// TestMain turns off the run ledger's fdatasync for this binary: the collector
// reads runs from a real ledger whose helpers append synced, and no test here
// checks durability. On a loaded shared runner a sync can exceed the ledger's
// 2s SyncTimeout, and the synced append failed with "not synced within 2s"
// (TestTransitionsAndSchedules, actions run 15479) exactly as it did in
// internal/scheduler before that binary got the same TestMain (#808). See
// ledger.SkipSyncForTesting.
func TestMain(m *testing.M) {
	ledger.SkipSyncForTesting()
	os.Exit(m.Run())
}
