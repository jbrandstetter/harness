package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/sealedlog"
)

// REQ-12's read side, with compression (ADR-0007 as amended): a closed run's
// log is compressed to <log>.zst, which is the same log, not a pruned one.
// Only once neither form exists does the record read log_pruned.
func TestCompressedLogIsNotPruned(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "1.log")
	if err := os.WriteFile(log, []byte("output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := openT(t, dir, Options{})
	ln := opened("a", 1, time.Now())
	ln.Log = log
	mustAppend(t, l, ln, true)
	if err := sealedlog.CompressFile(log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("the plain log is still there (err %v); the test would prove nothing", err)
	}
	if r, ok, _ := l.Get("a", 1); !ok || r.LogPruned {
		t.Errorf("a compressed log reads as pruned: %+v", r)
	}
	if recs, _, _ := l.Query(Query{Names: []string{"a"}}); len(recs) != 1 || recs[0].LogPruned {
		t.Errorf("Query reads a compressed log as pruned: %+v", recs)
	}
	_ = os.Remove(sealedlog.Compressed(log))
	if r, ok, _ := l.Get("a", 1); !ok || !r.LogPruned {
		t.Errorf("log_pruned not set once both forms are gone: %+v", r)
	}
}
