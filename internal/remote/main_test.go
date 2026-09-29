package remote

import (
	"os"
	"testing"

	"github.com/stump-wtf/harness/internal/ledger"
)

// TestMain turns off the run ledger's fdatasync for this binary: bootDaemon
// drives a real supervisor Manager, which opens a ledger, and none of these
// tests is about durability. See ledger.SkipSyncForTesting.
func TestMain(m *testing.M) {
	ledger.SkipSyncForTesting()
	os.Exit(m.Run())
}
