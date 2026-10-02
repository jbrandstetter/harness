// Serving-clone synchronization: `harness skills sync`, the only path that
// creates or fast-forwards a serving clone.
//
// The daemon never fetches, pulls, commits or pushes in a clone (SPEC-0007
// REQ "Default-Branch Gate"). This file runs in the CLI process: it performs
// the operator's git actions and returns which repos fast-forwarded, so the
// command can report them to the daemon over the control socket for reindex.
//
// Governing: ADR-0030 (serving clones synced by the client command);
// SPEC-0007 REQ "Default-Branch Gate", REQ "Merge and sync promote".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/gitcmd"
)

// SyncResult is the outcome of syncing one skill repo.
type SyncResult struct {
	// Repo is the skill repo's name.
	Repo string
	// FastForwarded is true when the clone was created or advanced to a new
	// default-branch commit. Only then does the daemon need a reindex.
	FastForwarded bool
	// Created is true when the serving clone did not exist and was cloned.
	Created bool
	// DefaultBranch is the branch the clone checks out.
	DefaultBranch string
	// Detail names a gate violation when the clone was not fast-forwarded
	// (detached HEAD or a dirty working tree).
	Detail string
}

// SyncRepo creates the serving clone when missing, or fast-forwards it to the
// remote's default branch. It never rebases, never force-updates and never
// touches a proposal branch: a clone that has diverged or is detached is
// reported as not fast-forwarded and left for the operator, keeping the
// daemon's last good index.
func SyncRepo(repoName, remote, cloneDir string) (SyncResult, error) {
	res := SyncResult{Repo: repoName}

	info, err := Inspect(cloneDir)
	if err != nil {
		return res, fmt.Errorf("skillserve: inspect %s: %w", repoName, err)
	}
	if info.State == CloneMissing {
		if out, err := gitcmd.Run("", "clone", remote, cloneDir); err != nil {
			return res, fmt.Errorf("skillserve: clone %s: %v: %s", repoName, err, out)
		}
		def := defaultBranch(cloneDir)
		return SyncResult{
			Repo:          repoName,
			FastForwarded: true,
			Created:       true,
			DefaultBranch: def,
		}, nil
	}

	def := info.DefaultBranch
	res.DefaultBranch = def
	if _, err := gitcmd.Run(cloneDir, "fetch", "origin"); err != nil {
		return res, fmt.Errorf("skillserve: fetch %s: %w", repoName, err)
	}
	// A detached or dirty clone must not be "fixed" by the sync command: the
	// operator chose that checkout, and the gate keeps the previous index
	// until it is back on the default branch and clean.
	if info.State != CloneOK {
		res.Detail = info.Detail
		return res, nil
	}
	// Up to date is not a fast-forward: the daemon has nothing to reindex.
	local, errL := gitRO(cloneDir, "rev-parse", "HEAD")
	remote, errR := gitRO(cloneDir, "rev-parse", "origin/"+def)
	if errL == nil && errR == nil && local == remote {
		return res, nil
	}
	if out, err := gitcmd.Run(cloneDir, "merge", "--ff-only", "origin/"+def); err != nil {
		return res, fmt.Errorf("skillserve: fast-forward %s: %v: %s", repoName, err, strings.TrimSpace(out))
	}
	res.FastForwarded = true
	return res, nil
}
