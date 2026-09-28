package daemon

import (
	"os"
	"testing"

	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/sealedlog"
)

// TestMain turns off the run ledger's fdatasync for this binary: every run
// these tests wait on is read back from the ledger, and none of them tests
// durability. See ledger.SkipSyncForTesting.
func TestMain(m *testing.M) {
	ledger.SkipSyncForTesting()
	// And the compression a sealed-log test waits on (sealed_logs_test.go).
	sealedlog.SkipSyncForTesting()
	os.Exit(m.Run())
}
