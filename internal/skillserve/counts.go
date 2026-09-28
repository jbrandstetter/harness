// The retrieval-count store: per (skill repo, skill, calling harness, run)
// retrieval records in daemon state, separate from the FTS5 index.
//
// Counts exist to ground retirement eligibility. They fail safe: a missing
// store is treated as "every skill newly promoted", so nothing becomes
// eligible for retirement until the grace period passes again. This package
// exposes eligibility only; the retire pull request itself is a separate
// story.
//
// Governing: SPEC-0007 REQ "Retrieval-Count Retirement", REQ "Database
// Operation Standards", REQ "Error Handling Standards"; ADR-0030.
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stump-wtf/harness/internal/supervisor"

	_ "modernc.org/sqlite"
)

// CountsStore records retrievals in its own database under the state
// directory, deliberately separate from the retrieval index: losing the index
// is a rebuildable cache miss, while losing counts must fail safe rather than
// silently reset a skill's retirement clock.
type CountsStore struct {
	db         *sql.DB
	wasMissing bool
}

// DefaultCountsPath returns the counts database path under the state
// directory: $XDG_STATE_HOME/harness/skills/counts.db.
func DefaultCountsPath() string {
	return filepath.Join(supervisor.StateHome(), "skills", "counts.db")
}

// OpenCounts opens (and creates) the retrieval-count database. wasMissing
// records whether the file had to be created: a fresh store means lost counts,
// so every skill is treated as newly promoted.
func OpenCounts(path string) (*CountsStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("skillserve: create %s: %w", filepath.Dir(path), err)
	}
	_, statErr := os.Stat(path)
	wasMissing := os.IsNotExist(statErr)

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
	if err != nil {
		return nil, fmt.Errorf("skillserve: open counts %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS retrievals (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			slug TEXT NOT NULL,
			harness TEXT NOT NULL,
			run TEXT NOT NULL DEFAULT '',
			at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_retrievals_skill ON retrievals(repo, slug, at)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("skillserve: migrate counts: %w", err)
		}
	}
	return &CountsStore{db: db, wasMissing: wasMissing}, nil
}

// Close releases the database connection.
func (c *CountsStore) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// WasMissing reports whether the store had to be created at open. While true,
// no skill is eligible for retirement (SPEC-0007 REQ "Retrieval-Count
// Retirement": lost counts fail safe).
func (c *CountsStore) WasMissing() bool {
	return c == nil || c.wasMissing
}

// Record counts one retrieval of a skill. harness attributes the calling
// harness and run the run it happened in; both are opaque identifiers.
func (c *CountsStore) Record(repo, slug, harness, run string, at time.Time) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("skillserve: counts store not open")
	}
	if _, err := c.db.Exec(
		`INSERT INTO retrievals (repo, slug, harness, run, at) VALUES (?, ?, ?, ?, ?)`,
		repo, slug, harness, run, at.Unix()); err != nil {
		return fmt.Errorf("skillserve: record retrieval %s/%s: %w", repo, slug, err)
	}
	return nil
}

// CountSince returns how many retrievals a skill had since the given time.
// A nil or closed store counts as zero retrievals.
func (c *CountsStore) CountSince(repo, slug string, since time.Time) (int, error) {
	if c == nil || c.db == nil {
		return 0, nil
	}
	var n int
	if err := c.db.QueryRow(
		`SELECT COUNT(*) FROM retrievals WHERE repo = ? AND slug = ? AND at >= ?`,
		repo, slug, since.Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("skillserve: count retrievals %s/%s: %w", repo, slug, err)
	}
	return n, nil
}

// EligibleForRetirement reports whether a skill may receive a retire pull
// request. Eligibility requires the skill to be past its grace period since
// merge, to have zero retrievals inside the window, and a count store that was
// not just recreated (lost counts fail safe). The retire pull request itself
// is not part of this decision.
func (c *CountsStore) EligibleForRetirement(repo, slug string, mergedAt time.Time, now time.Time, graceDays, windowDays int) (bool, error) {
	if graceDays < 0 {
		graceDays = 0
	}
	if windowDays < 0 {
		windowDays = 0
	}
	if c.WasMissing() {
		return false, nil
	}
	if now.Sub(mergedAt) < time.Duration(graceDays)*24*time.Hour {
		return false, nil
	}
	n, err := c.CountSince(repo, slug, now.Add(-time.Duration(windowDays)*24*time.Hour))
	if err != nil {
		return false, err
	}
	return n == 0, nil
}
