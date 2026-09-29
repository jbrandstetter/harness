package scheduler

import (
	"os"
	"testing"

	"github.com/stump-wtf/harness/internal/ledger"
)

// TestMain turns off the run ledger's fdatasync for this binary: the gate
// tests that drive a real supervisor Manager wait on runs it records, and
// none of them tests durability. See ledger.SkipSyncForTesting.
func TestMain(m *testing.M) {
	ledger.SkipSyncForTesting()
	os.Exit(m.Run())
}
