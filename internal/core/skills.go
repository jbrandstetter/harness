// Skill-repo serving configuration: the [skills] table and the
// [skill_repo.<name>] tables, global-only concerns owned by the daemon.
//
// Governing: SPEC-0007 REQ "Skill Repos", REQ "Default-Branch Gate",
// REQ "Retrieval-Count Retirement"; ADR-0030 (grounded skill distillation).
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package core

import ()

// Defaults for the [skills] table (SPEC-0007 REQ "Skill Repos").
const (
	// DefaultSummaryMaxChars caps get_skill's summary render.
	DefaultSummaryMaxChars = 1000
	// DefaultRetireGraceDays protects a newly merged skill from retirement.
	DefaultRetireGraceDays = 30
	// DefaultRetireWindowDays is the zero-retrieval window after grace.
	DefaultRetireWindowDays = 60
	// DefaultSkillRepoPath is where skills live inside a skill repo when
	// `path` is omitted.
	DefaultSkillRepoPath = "skills"
)

// SkillsConfig is the optional global [skills] table. Every field is a
// pointer so the zero value means "unset"; the Resolved accessors apply the
// defaults.
type SkillsConfig struct {
	// SummaryMaxChars caps get_skill's summary render.
	SummaryMaxChars *int
	// RetireGraceDays is how long after merge a skill cannot be retired.
	RetireGraceDays *int
	// RetireWindowDays is the zero-retrieval window after the grace period.
	RetireWindowDays *int
}

// SummaryMaxCharsOrDefault applies the default.
func (s SkillsConfig) SummaryMaxCharsOrDefault() int {
	if s.SummaryMaxChars != nil {
		return *s.SummaryMaxChars
	}
	return DefaultSummaryMaxChars
}

// RetireGraceDaysOrDefault applies the default.
func (s SkillsConfig) RetireGraceDaysOrDefault() int {
	if s.RetireGraceDays != nil {
		return *s.RetireGraceDays
	}
	return DefaultRetireGraceDays
}

// RetireWindowDaysOrDefault applies the default.
func (s SkillsConfig) RetireWindowDaysOrDefault() int {
	if s.RetireWindowDays != nil {
		return *s.RetireWindowDays
	}
	return DefaultRetireWindowDays
}

// SkillRepo is one declared [skill_repo.<name>] table: a forge repository
// whose serving clone the daemon indexes for search.
type SkillRepo struct {
	// Name is the table suffix, unique across the config.
	Name string
	// Remote is the forge URL `harness skills sync` clones and fast-forwards.
	Remote string
	// Path is the directory inside the repo holding the skills. Defaults to
	// "skills".
	Path string
	// ServeTo is the harness-selector list scoping which harnesses may
	// search the repo. Defaults to ["*"].
	ServeTo []string
	// Public is true when unset: an unset `public` is treated as true.
	Public bool
}

// ServeToOrDefault applies the default.
func (r SkillRepo) ServeToOrDefault() []string {
	if len(r.ServeTo) > 0 {
		return r.ServeTo
	}
	return []string{"*"}
}

// OrderedSkillRepos returns the declared skill repos in file order.
func (c *Config) OrderedSkillRepos() []SkillRepo {
	if len(c.SkillRepoOrder) == 0 {
		return nil
	}
	out := make([]SkillRepo, 0, len(c.SkillRepoOrder))
	for _, name := range c.SkillRepoOrder {
		out = append(out, c.SkillRepos[name])
	}
	return out
}
