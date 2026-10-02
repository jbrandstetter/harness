// Package skillserve serves declared skill repos to search: it inspects the
// serving clones, keeps an embedded FTS5 retrieval index over the active
// skills, and counts retrievals for retirement eligibility.
//
// # Serving Clones And The Default-Branch Gate
//
// Each declared [skill_repo.<name>] gets a serving clone at
// $XDG_STATE_HOME/harness/skills/<name>. Only `harness skills sync` creates
// and fast-forwards a clone (sync.go); the daemon's only git access here is
// read-only inspection, and nothing in this package ever fetches, pulls,
// commits or pushes. The daemon indexes only files under the repo's `path` on
// the checked-out default branch. A detached-HEAD or dirty clone keeps the
// previous index and is surfaced through the repo status, which `harness
// doctor` reports.
//
// # Embedded Retrieval Index
//
// The index is a modernc.org/sqlite FTS5 table over name, description,
// symptoms, tags and applies_to with porter stemming, ranked with bm25(). It
// is a rebuildable cache: deleting it and reindexing the serving clones
// restores equivalent behavior. A full repo reindex runs in one transaction, so
// a failure partway leaves the previous index queryable. One malformed skill
// does not break a reindex: the rest index and the file is reported.
//
// # Retrieval Counts
//
// Every retrieval is counted per (skill repo, skill, calling harness, run) in
// a separate daemon-state store. Lost counts fail safe: with a missing store
// nothing is eligible for retirement until the grace period passes again.
// This package only exposes retirement *eligibility*; the retire pull request
// itself is a separate story.
//
// Governing: ADR-0030 (grounded skill distillation), ADR-0012 (search-only
// learned tier); SPEC-0007 REQ "Default-Branch Gate", REQ "Embedded Retrieval
// Index", REQ "Retrieval-Count Retirement", REQ "Database Operation
// Standards", REQ "Error Handling Standards", REQ "Skill Repos".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stump-wtf/harness/internal/gitcmd"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// CloneDir returns the serving clone path for a skill repo name:
// $XDG_STATE_HOME/harness/skills/<name>.
func CloneDir(name string) string {
	return filepath.Join(supervisor.StateHome(), "skills", name)
}

// CloneState is the default-branch-gate state of a serving clone.
type CloneState string

const (
	// CloneMissing: no clone exists at the path yet.
	CloneMissing CloneState = "missing"
	// CloneOK: HEAD is the default branch and the working tree is clean.
	CloneOK CloneState = "ok"
	// CloneDetached: HEAD is not the default branch (a detached HEAD or a
	// proposal branch).
	CloneDetached CloneState = "detached"
	// CloneDirty: HEAD is the default branch but the working tree is dirty.
	CloneDirty CloneState = "dirty"
	// CloneIndexError: the clone passed the gate but its reindex failed (a
	// database error, not a clone condition). The previous index keeps
	// serving; the detail names the cause.
	CloneIndexError CloneState = "index_error"
)

// CloneInfo is the result of one read-only inspection of a serving clone.
type CloneInfo struct {
	// State is the gate state above.
	State CloneState
	// DefaultBranch is the repo's default branch as far as the clone records
	// it (origin/HEAD), empty when unknown.
	DefaultBranch string
	// Head is the checked-out branch or, when detached, "(detached)".
	Head string
	// Detail is the human-readable gate violation, empty when State is OK.
	Detail string
}

// gitRO runs a read-only git command in dir. Nothing here mutates a clone.
func gitRO(dir string, args ...string) (string, error) {
	return gitcmd.Output(dir, args...)
}

// defaultBranch reads the clone's recorded default branch (origin/HEAD),
// falling back to "main".
func defaultBranch(dir string) string {
	ref, err := gitRO(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil || ref == "" {
		return "main"
	}
	return strings.TrimPrefix(ref, "origin/")
}

// Inspect inspects a serving clone read-only: which branch HEAD names, whether
// the working tree is dirty, and the clone's default branch. It never fetches,
// pulls, commits or pushes (SPEC-0007 REQ "Default-Branch Gate").
func Inspect(dir string) (CloneInfo, error) {
	if _, err := gitRO(dir, "rev-parse", "--git-dir"); err != nil {
		return CloneInfo{State: CloneMissing}, nil
	}

	def := defaultBranch(dir)
	head, err := gitRO(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		// A detached HEAD has no symbolic ref.
		info := CloneInfo{State: CloneDetached, DefaultBranch: def, Head: "(detached)"}
		info.Detail = fmt.Sprintf("HEAD is detached, not on the default branch %q", def)
		return info, nil
	}
	if head != def {
		info := CloneInfo{State: CloneDetached, DefaultBranch: def, Head: head}
		info.Detail = fmt.Sprintf("HEAD is on %q, not the default branch %q", head, def)
		return info, nil
	}

	status, err := gitRO(dir, "status", "--porcelain")
	if err != nil {
		return CloneInfo{}, fmt.Errorf("skillserve: inspect %s: %w", dir, err)
	}
	if status != "" {
		info := CloneInfo{State: CloneDirty, DefaultBranch: def, Head: head}
		info.Detail = "the clone's working tree is dirty"
		return info, nil
	}
	return CloneInfo{State: CloneOK, DefaultBranch: def, Head: head}, nil
}

// MergedAt returns the commit time of a file's last change on the current
// checkout, read-only. It backs retirement eligibility (SPEC-0007 REQ
// "Retrieval-Count Retirement": grace days run from the merge). The zero time
// is returned when the file has no commit history.
func MergedAt(dir, relPath string) (int64, error) {
	out, err := gitRO(dir, "log", "-1", "--format=%ct", "--", relPath)
	if err != nil || out == "" {
		return 0, err
	}
	var secs int64
	if _, err := fmt.Sscanf(out, "%d", &secs); err != nil {
		return 0, nil
	}
	return secs, nil
}
