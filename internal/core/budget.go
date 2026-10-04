package core

// Run Budgets
//
// The parsed shapes of SPEC-0021's budget configuration: the per-harness
// budget keys (Budget, on Harness) and the daemon-wide [budget] table
// (DaemonBudget, on Config). internal/config validates both at load with
// located errors; nothing here enforces anything. Admission (#470), quota
// parking (#477), concurrency (#479) and the meter-driven caps (#482) read
// these values in later stories.
//
// Every zero value means "unset": a harness with a zero Budget behaves exactly
// as it did before SPEC-0021 (REQ-1), and a zero DaemonBudget is a budget day
// starting at 00:00 in the daemon's zone with no daemon-wide caps.
//
// Governing: ADR-0027 (run budgets and usage-limit backoff); SPEC-0021 REQ-1
// "Per-harness budget keys", REQ-2 "The [budget] table"; run-budgets
// design.md § "Config shapes".
//
// @joestump 10/04/2026 - Added for #465.

import (
	"regexp"
	"time"

	"github.com/stump-wtf/harness/internal/hours"
)

const (
	// DefaultQuotaBackoff is the first park length when a quota refusal
	// gives no reset time and the harness sets no quota_backoff (SPEC-0021
	// REQ-1).
	DefaultQuotaBackoff = 15 * time.Minute
	// DefaultQuotaBackoffMax is the longest backoff park when the harness
	// sets no quota_backoff_max (SPEC-0021 REQ-1).
	DefaultQuotaBackoffMax = 6 * time.Hour
	// MinQuotaBackoff is the shortest quota_backoff a harness may set.
	MinQuotaBackoff = time.Minute
	// MaxQuotaBackoffMax is the longest quota_backoff_max a harness may set
	// (SPEC-0021 REQ-1). It bounds a backoff park, which is a guess made
	// without a reset time; a park on a parsed reset time has its own,
	// longer clamp (REQ-12).
	MaxQuotaBackoffMax = 24 * time.Hour
)

// quotaGroupRe is SPEC-0021 REQ-1's quota_group shape, which also names a
// [budget.group.<name>] table.
var quotaGroupRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)

// ValidQuotaGroup reports whether name is a legal quota_group, and so a legal
// [budget.group.<name>] name: 1 to 64 of a-z, 0-9, ".", "_" and "-".
func ValidQuotaGroup(name string) bool { return quotaGroupRe.MatchString(name) }

// Budget is a harness's budget keys (SPEC-0021 REQ-1), global config only: a
// project harness.toml and a harness_d drop-in reject every one of them. Each
// field is the value the operator wrote, zero when the key is absent; the two
// backoff durations resolve their defaults through the Effective* methods, so
// a harness built off the wire rather than by the parser gets the same
// defaults as one that was parsed.
type Budget struct {
	// MaxRunsPerDay is how many admissions this harness gets per budget
	// day. For a resident harness every process start counts, restarts
	// included. 0 means unset.
	MaxRunsPerDay int
	// MaxTokens is the most tokens one run may spend (input + output +
	// cache writes). A per-run cap, so it is accepted only on a one-shot
	// (prompt or prompt_file set). 0 means unset.
	MaxTokens int64
	// MaxCostUSD is the most dollars one run may spend. Per-run, one-shot
	// only, like MaxTokens. 0 means unset.
	MaxCostUSD float64
	// DailyCostUSD is the most dollars this harness may spend per budget
	// day. 0 means unset.
	DailyCostUSD float64
	// QuotaGroup names the provider allowance this harness shares with every
	// other harness carrying the same value: a quota park on one member parks
	// them all. "" means the harness parks alone.
	QuotaGroup string
	// QuotaBackoff is the first park length when a quota refusal gives no
	// reset time. 0 means DefaultQuotaBackoff; read EffectiveQuotaBackoff.
	QuotaBackoff time.Duration
	// QuotaBackoffMax is the longest backoff park. 0 means
	// DefaultQuotaBackoffMax; read EffectiveQuotaBackoffMax.
	QuotaBackoffMax time.Duration
}

// EffectiveQuotaBackoff is QuotaBackoff, or DefaultQuotaBackoff when unset.
func (b Budget) EffectiveQuotaBackoff() time.Duration {
	if b.QuotaBackoff > 0 {
		return b.QuotaBackoff
	}
	return DefaultQuotaBackoff
}

// EffectiveQuotaBackoffMax is QuotaBackoffMax, or DefaultQuotaBackoffMax
// when unset.
func (b Budget) EffectiveQuotaBackoffMax() time.Duration {
	if b.QuotaBackoffMax > 0 {
		return b.QuotaBackoffMax
	}
	return DefaultQuotaBackoffMax
}

// DaemonBudget is the global [budget] table (SPEC-0021 REQ-2). A project
// harness.toml and a harness_d drop-in reject it.
type DaemonBudget struct {
	// DayStarts is when each budget day begins. The zero value is 00:00 in
	// the daemon's zone, the default for an absent day_starts.
	DayStarts hours.DailyInstant
	// MaxConcurrent caps the triggered one-shot runs in flight at once,
	// daemon-wide. 0 means unset.
	MaxConcurrent int
	// DailyCostUSD is the most dollars every harness together may spend per
	// budget day. 0 means unset.
	DailyCostUSD float64
	// Prices are the operator's per-million-token prices, keyed by the
	// served model name. Nil when none are declared.
	Prices map[string]Price
	// Groups are the [budget.group.<name>] tables, keyed by quota group
	// name. Nil when none are declared. A group no harness names still
	// loads; doctor is where that gets flagged (#486).
	Groups map[string]GroupBudget
}

// Price is one [budget.prices.<model>] table: dollars per million tokens.
// Input and output are required; the cache prices default to 0.
type Price struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheWritePerMTok float64
	CacheReadPerMTok  float64
}

// GroupBudget is one [budget.group.<name>] table.
type GroupBudget struct {
	// MaxConcurrent caps the triggered runs in flight at once among the
	// harnesses whose quota_group is this group's name.
	MaxConcurrent int
}
