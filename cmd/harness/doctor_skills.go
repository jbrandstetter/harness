package main

// Doctor: skill repo serving clones (SPEC-0007).
//
// A serving clone that is off the default branch, dirty, missing, or that
// reindexed with malformed skill files keeps serving its previous index, so
// the daemon's searches stay correct while the condition persists. This check
// surfaces each condition so the operator can fix the clone (or run `harness
// skills sync`) instead of discovering a stale index later.
//
// Governing: SPEC-0007 REQ "Default-Branch Gate", REQ "Error Handling
// Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#78.

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

// skillsCheck reports the default-branch-gate state of every declared skill
// repo. It returns nil when no skill repo is declared (nothing to check) or
// the daemon could not be asked (the daemon row already carries that).
func skillsCheck(cfg *core.Config, c skillsStatusClient) *check {
	if cfg == nil || len(cfg.SkillRepos) == 0 {
		return nil
	}
	st, err := c.SkillsStatus()
	if err != nil {
		// An unanswered daemon is not evidence a clone is broken.
		return nil
	}

	var problems []string
	var hints []string
	okCount := 0
	for _, r := range st.Repos {
		switch r.State {
		case "ok":
			okCount++
		case "missing":
			problems = append(problems, fmt.Sprintf("%s: no serving clone", r.Name))
			hints = append(hints, "harness skills sync")
		case "detached":
			problems = append(problems, fmt.Sprintf("%s: %s", r.Name, r.Detail))
			hints = append(hints, "check out "+firstNonEmpty(defaultBranchHint(r), "the default branch")+" in the serving clone")
		case "dirty":
			problems = append(problems, fmt.Sprintf("%s: %s", r.Name, r.Detail))
			hints = append(hints, "clean or stash the serving clone's working tree")
		default:
			problems = append(problems, fmt.Sprintf("%s: %s", r.Name, firstNonEmpty(r.Detail, r.State)))
			hints = append(hints, "harness skills sync")
		}
		for _, w := range r.Warnings {
			problems = append(problems, fmt.Sprintf("%s: %s", r.Name, w))
			hints = append(hints, "fix the named skill file")
		}
	}
	if len(problems) == 0 {
		return &check{
			name:   "skills",
			level:  cliui.LevelSuccess,
			detail: fmt.Sprintf("%d skill repo(s) serving, %d skill(s) indexed", len(st.Repos), totalSkills(st.Repos)),
		}
	}
	seenHint := map[string]bool{}
	var uniqHints []string
	for _, h := range hints {
		if !seenHint[h] {
			seenHint[h] = true
			uniqHints = append(uniqHints, h)
		}
	}
	return &check{
		name:   "skills",
		level:  cliui.LevelWarn,
		detail: strings.Join(problems, "; "),
		hint:   strings.Join(uniqHints, "; "),
	}
}

// skillsStatusClient is the daemon call the check needs, so tests can stub it.
type skillsStatusClient interface {
	SkillsStatus() (protocol.SkillsStatusData, error)
}

func totalSkills(repos []protocol.SkillRepoStatus) int {
	n := 0
	for _, r := range repos {
		n += r.Skills
	}
	return n
}

func defaultBranchHint(r protocol.SkillRepoStatus) string {
	// The daemon's detail names both branches; keep the hint generic rather
	// than parsing it back out.
	return "the default branch"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
