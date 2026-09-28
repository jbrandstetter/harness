// Skills and Skill Repo Tables
//
// Parses and validates the global [skills] table and the [skill_repo.<name>]
// tables into core.SkillsConfig and core.SkillRepo. These are daemon-owned
// serving concerns: a project harness.toml is refused them the same way it is
// refused [server] and [telemetry], so a cloned repository cannot declare its
// own skill repo.
//
// Governing: SPEC-0007 REQ "Skill Repos" ("Skill repo and [skills] tables
// SHALL be accepted in global configuration only"); ADR-0030.
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package config

import (
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// rawSkills mirrors the [skills] table before validation. The tunings are
// pointers so an absent key takes its default and an explicit nonsensical
// value is refused rather than silently accepted.
type rawSkills struct {
	SummaryMaxChars  *int `toml:"summary_max_chars"`
	RetireGraceDays  *int `toml:"retire_grace_days"`
	RetireWindowDays *int `toml:"retire_window_days"`
}

// rawSkillRepo mirrors one [skill_repo.<name>] table before validation.
// Public is a pointer so an unset key resolves to true rather than false.
type rawSkillRepo struct {
	Remote  string    `toml:"remote"`
	Path    *string   `toml:"path"`
	ServeTo *[]string `toml:"serve_to"`
	Public  *bool     `toml:"public"`
}

// buildSkills validates rs into a core.SkillsConfig.
func buildSkills(filename string, line int, rs rawSkills) (core.SkillsConfig, error) {
	fail := func(key, format string, args ...any) (core.SkillsConfig, error) {
		return core.SkillsConfig{}, newError(filename, line, "[skills] %q: "+format, append([]any{key}, args...)...)
	}

	sc := core.SkillsConfig{
		SummaryMaxChars:  rs.SummaryMaxChars,
		RetireGraceDays:  rs.RetireGraceDays,
		RetireWindowDays: rs.RetireWindowDays,
	}
	if rs.SummaryMaxChars != nil && (*rs.SummaryMaxChars < 200 || *rs.SummaryMaxChars > 5000) {
		return fail("summary_max_chars", "must be between 200 and 5000 (got %d)", *rs.SummaryMaxChars)
	}
	if rs.RetireGraceDays != nil && *rs.RetireGraceDays < 0 {
		return fail("retire_grace_days", "must be 0 or more (got %d)", *rs.RetireGraceDays)
	}
	if rs.RetireWindowDays != nil && *rs.RetireWindowDays < 0 {
		return fail("retire_window_days", "must be 0 or more (got %d)", *rs.RetireWindowDays)
	}
	if rs.RetireGraceDays != nil && rs.RetireWindowDays != nil && *rs.RetireWindowDays < *rs.RetireGraceDays {
		return fail("retire_window_days", "must not be shorter than retire_grace_days (%d < %d)", *rs.RetireWindowDays, *rs.RetireGraceDays)
	}
	return sc, nil
}

// addSkillRepo validates one raw skill repo table and registers it on cfg.
func addSkillRepo(cfg *core.Config, filename, name string, line int, rr rawSkillRepo) error {
	fail := func(format string, args ...any) error {
		return newError(filename, line, "[skill_repo.%s]: "+format, append([]any{name}, args...)...)
	}

	if strings.Contains(name, "/") || strings.Contains(name, ".") {
		return fail("name must not contain \"/\" or \".\"")
	}
	if strings.TrimSpace(rr.Remote) == "" {
		return fail("remote is required: the forge URL of the skill repository")
	}
	path := core.DefaultSkillRepoPath
	if rr.Path != nil {
		path = strings.TrimSpace(*rr.Path)
		if path == "" {
			return fail("path must not be empty (omit it for the default %q)", core.DefaultSkillRepoPath)
		}
	}
	serveTo := []string{"*"}
	if rr.ServeTo != nil {
		serveTo = *rr.ServeTo
		if len(serveTo) == 0 {
			return fail("serve_to must not be empty (omit it to serve every harness)")
		}
		for _, sel := range serveTo {
			if strings.TrimSpace(sel) == "" {
				return fail("serve_to must not contain empty selectors")
			}
		}
	}
	public := true
	if rr.Public != nil {
		public = *rr.Public
	}
	if _, exists := cfg.SkillRepos[name]; exists {
		return fail("duplicate skill repo %q", name)
	}
	if cfg.SkillRepos == nil {
		cfg.SkillRepos = map[string]core.SkillRepo{}
	}
	cfg.SkillRepos[name] = core.SkillRepo{
		Name:    name,
		Remote:  strings.TrimSpace(rr.Remote),
		Path:    path,
		ServeTo: serveTo,
		Public:  public,
	}
	cfg.SkillRepoOrder = append(cfg.SkillRepoOrder, name)
	return nil
}
