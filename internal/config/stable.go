// Stable Tables
//
// Parses and validates the global [stable.<name>] tables into core.Stable.
// The table is the SPEC-0026 trust ledger: a project harness.toml is refused
// it exactly as it is refused [skill_repo.*], because a cloned repository
// must not be able to expand what is trusted on the machine that clones it.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-1 (stable
// registration and global-only trust).
//
// @joestump-agent 10/02/2026 - Added for harness#811.
package config

import (
	"strings"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/core"
)

// rawStable mirrors one [stable.<name>] table before validation. Public is a
// pointer so an unset key resolves to true rather than false, the skill_repo
// convention (SPEC-0026 REQ-1).
type rawStable struct {
	Remote string `toml:"remote"`
	Public *bool  `toml:"public"`
}

// addStable validates one raw stable table and registers it on cfg. The name
// grammar is the shared stable/package grammar (agentpkg.NamePattern).
func addStable(cfg *core.Config, filename, name string, line int, rs rawStable) error {
	fail := func(format string, args ...any) error {
		return newError(filename, line, "[stable.%s]: "+format, append([]any{name}, args...)...)
	}

	if !agentpkg.NamePattern.MatchString(name) {
		return fail("name must match %s", agentpkg.NamePattern)
	}
	if strings.TrimSpace(rs.Remote) == "" {
		return fail("remote is required: the git URL of the stable")
	}
	public := true
	if rs.Public != nil {
		public = *rs.Public
	}
	if _, exists := cfg.Stables[name]; exists {
		return fail("duplicate stable %q", name)
	}
	if cfg.Stables == nil {
		cfg.Stables = map[string]core.Stable{}
	}
	cfg.Stables[name] = core.Stable{
		Name:    name,
		Remote:  strings.TrimSpace(rs.Remote),
		Public:  public,
	}
	cfg.StableOrder = append(cfg.StableOrder, name)
	return nil
}
