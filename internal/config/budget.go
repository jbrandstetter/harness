package config

// Run Budgets
//
// Parses and validates SPEC-0021's budget configuration: the per-harness
// budget keys into core.Budget, and the global [budget] table into
// core.DaemonBudget. Nothing is enforced here, or yet anywhere: admission
// (#470), quota parking (#477), concurrency (#479) and the meter-driven caps
// (#482) read these values. What this file owns is that a budget the daemon
// will one day enforce is never a silently wrong one today, so every key is
// checked at load with the harness, the key and the key's own line.
//
// Every key is decoded untyped and type-checked here rather than by the TOML
// decoder. A decoder type error reports the table's header line, not the
// key's, and `max_runs_per_day = "40"` deserves the line it is on.
//
// Budgets are global-only. A project harness.toml and a harness_d drop-in
// reject both the per-harness keys and [budget]: the run budget is the
// operator's spend, and neither a cloned repository nor a unit dropped in
// beside the config gets to raise it (design.md § "Non-Goals" defers project
// budgets the way ADR-0019 deferred project hours).
//
// Governing: ADR-0027; SPEC-0021 REQ-1 "Per-harness budget keys", REQ-2 "The
// [budget] table"; run-budgets design.md § "Config shapes" ("Validation lives
// beside operatinghours validation in internal/config, with located errors
// via lineOfKeyInTable").
//
// @joestump 10/04/2026 - Added for #465.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

// rawBudget mirrors [budget] before validation. Prices and groups may be
// written as sub-tables ([budget.prices."gpt-5"]), inline tables, or dotted
// keys; the decoder folds all three into these maps.
type rawBudget struct {
	DayStarts     any                 `toml:"day_starts"`
	MaxConcurrent any                 `toml:"max_concurrent"`
	DailyCostUSD  any                 `toml:"daily_cost_usd"`
	Prices        map[string]rawPrice `toml:"prices"`
	Group         map[string]rawGroup `toml:"group"`
}

// rawPrice mirrors one [budget.prices.<model>] table.
type rawPrice struct {
	InputPerMTok      any `toml:"input_per_mtok"`
	OutputPerMTok     any `toml:"output_per_mtok"`
	CacheWritePerMTok any `toml:"cache_write_per_mtok"`
	CacheReadPerMTok  any `toml:"cache_read_per_mtok"`
}

// rawGroup mirrors one [budget.group.<name>] table.
type rawGroup struct {
	MaxConcurrent any `toml:"max_concurrent"`
}

// budgetKeys returns the per-harness budget keys rh sets, in the order
// SPEC-0021 REQ-1 lists them, so the project-file and drop-in refusals name
// the first one deterministically.
func (rh rawHarness) budgetKeys() []string {
	var out []string
	for _, k := range []struct {
		key string
		v   any
	}{
		{"max_runs_per_day", rh.MaxRunsPerDay},
		{"max_tokens", rh.MaxTokens},
		{"max_cost_usd", rh.MaxCostUSD},
		{"daily_cost_usd", rh.DailyCostUSD},
		{"quota_group", rh.QuotaGroup},
		{"quota_backoff", rh.QuotaBackoff},
		{"quota_backoff_max", rh.QuotaBackoffMax},
	} {
		if k.v != nil {
			out = append(out, k.key)
		}
	}
	return out
}

// lineOfKey is the line of key inside rh's own table, or fallback (the
// table's header line) when the caller recorded no lookup or the key cannot
// be found, e.g. in an inline table.
func (rh rawHarness) lineOfKey(key string, fallback int) int {
	if rh.keyLine != nil {
		if l := rh.keyLine(key); l > 0 {
			return l
		}
	}
	return fallback
}

// keyLineIn returns a key-line lookup for one table of data, for
// rawHarness.keyLine.
func keyLineIn(data []byte, table string) func(string) int {
	return func(key string) int { return lineOfKeyInTable(data, table, key) }
}

// budgetOnlyGlobalErr is the refusal of a budget key outside the global
// config. where is "a project file" or "a harness_d drop-in".
func budgetOnlyGlobalErr(filename string, line int, name, key, where string) *Error {
	return newError(filename, line,
		"harness %q: %q is not accepted in %s (budgets are only accepted in the global config: the daemon's harness.toml itself)",
		name, key, where)
}

// buildHarnessBudget validates rh's budget keys into a core.Budget. isAgent
// is registerHarness's one-shot predicate (prompt or prompt_file set), which
// the per-run caps require.
func buildHarnessBudget(filename, name string, line int, rh rawHarness, isAgent bool) (core.Budget, error) {
	fail := func(key, format string, args ...any) (core.Budget, error) {
		return core.Budget{}, newError(filename, rh.lineOfKey(key, line),
			"harness %q: %q "+format, append([]any{name, key}, args...)...)
	}

	// A resident's "run" is a process lifetime that can last for days, so a
	// per-run cap there bounds nothing. Refused on presence, before the value
	// is looked at, like every other exclusion in registerHarness.
	// Governing: SPEC-0021 REQ-1 scenario "A per-run cap on a resident
	// harness"; ADR-0027 § "The schema".
	for _, k := range []struct {
		key string
		v   any
	}{{"max_tokens", rh.MaxTokens}, {"max_cost_usd", rh.MaxCostUSD}} {
		if k.v != nil && !isAgent {
			return fail(k.key,
				"is a per-run cap, and per-run caps apply only to one-shot harnesses (set \"prompt\" or \"prompt_file\"); bound a resident harness with \"max_runs_per_day\" and \"daily_cost_usd\"")
		}
	}

	var b core.Budget
	if v := rh.MaxRunsPerDay; v != nil {
		n, ok := budgetWhole(v)
		if !ok || n < 1 {
			return fail("max_runs_per_day", "must be a whole number of at least 1 (got %s)", describeTOMLValue(v))
		}
		b.MaxRunsPerDay = n
	}
	if v := rh.MaxTokens; v != nil {
		n, ok := v.(int64)
		if !ok || n < 1 {
			return fail("max_tokens", "must be a whole number of at least 1 (got %s)", describeTOMLValue(v))
		}
		b.MaxTokens = n
	}
	if v := rh.MaxCostUSD; v != nil {
		f, ok := budgetDecimal(v)
		if !ok || f <= 0 {
			return fail("max_cost_usd", "must be a number of dollars greater than 0 (got %s)", describeTOMLValue(v))
		}
		b.MaxCostUSD = f
	}
	if v := rh.DailyCostUSD; v != nil {
		f, ok := budgetDecimal(v)
		if !ok || f <= 0 {
			return fail("daily_cost_usd", "must be a number of dollars greater than 0 (got %s)", describeTOMLValue(v))
		}
		b.DailyCostUSD = f
	}
	if v := rh.QuotaGroup; v != nil {
		s, ok := v.(string)
		if !ok || !core.ValidQuotaGroup(s) {
			return fail("quota_group", "must be 1 to 64 of a-z, 0-9, \".\", \"_\" and \"-\" (got %s)", describeTOMLValue(v))
		}
		b.QuotaGroup = s
	}
	if v := rh.QuotaBackoff; v != nil {
		d, ok := budgetDuration(v)
		if !ok || d < core.MinQuotaBackoff {
			return fail("quota_backoff", "must be a duration of at least 1m, such as \"15m\" (got %s)", describeTOMLValue(v))
		}
		b.QuotaBackoff = d
	}
	if v := rh.QuotaBackoffMax; v != nil {
		d, ok := budgetDuration(v)
		if !ok || d <= 0 || d > core.MaxQuotaBackoffMax {
			return fail("quota_backoff_max", "must be a duration no longer than 24h, such as \"6h\" (got %s)", describeTOMLValue(v))
		}
		b.QuotaBackoffMax = d
	}
	// The ordering rule is checked on the effective values, defaults
	// included: quota_backoff = "8h" alone is past the 6h default ceiling,
	// and loading it would make the first park longer than the longest.
	if b.EffectiveQuotaBackoff() > b.EffectiveQuotaBackoffMax() {
		switch {
		case rh.QuotaBackoffMax == nil:
			return fail("quota_backoff", "must not exceed quota_backoff_max, which defaults to 6h; set quota_backoff_max too (got %s)",
				describeTOMLValue(rh.QuotaBackoff))
		case rh.QuotaBackoff == nil:
			return fail("quota_backoff_max", "must be at least quota_backoff, which defaults to 15m (got %s)",
				describeTOMLValue(rh.QuotaBackoffMax))
		default:
			return fail("quota_backoff_max", "must be at least quota_backoff (%s) (got %s)",
				describeTOMLValue(rh.QuotaBackoff), describeTOMLValue(rh.QuotaBackoffMax))
		}
	}
	return b, nil
}

// buildDaemonBudget validates a decoded [budget] table into a
// core.DaemonBudget. line is the [budget] header's line (or the first
// [budget.*] header's), the fallback for a key whose own line cannot be
// found.
func buildDaemonBudget(filename string, data []byte, line int, rb rawBudget) (core.DaemonBudget, error) {
	headers := scanTables(data)
	// keyLine locates key in table, falling back to the table's header and
	// then to [budget]'s: a price written inline or as dotted keys has no
	// header of its own.
	keyLine := func(table, key string) int {
		if l := lineOfKeyInTable(data, table, key); l > 0 {
			return l
		}
		if l := lineOf(headers, table); l > 0 {
			return l
		}
		return line
	}
	fail := func(table, key, format string, args ...any) (core.DaemonBudget, error) {
		return core.DaemonBudget{}, newError(filename, keyLine(table, key),
			"[%s] %q: "+format, append([]any{budgetTableLabel(table), key}, args...)...)
	}

	var db core.DaemonBudget
	if v := rb.DayStarts; v != nil {
		s, ok := v.(string)
		if !ok {
			return fail("budget", "day_starts", "must be a string such as \"TZ=America/Los_Angeles 00:00\" (got %s)", describeTOMLValue(v))
		}
		// The zone prefix is operating_hours' own parser, so an unknown zone
		// fails with SPEC-0008's error (REQ-2 "An unknown zone").
		d, err := hours.ParseDailyInstant(s)
		if err != nil {
			return fail("budget", "day_starts", "invalid %q: %v", s, err)
		}
		db.DayStarts = d
	}
	if v := rb.MaxConcurrent; v != nil {
		n, ok := budgetWhole(v)
		if !ok || n < 1 {
			return fail("budget", "max_concurrent", "must be a whole number of at least 1 (got %s)", describeTOMLValue(v))
		}
		db.MaxConcurrent = n
	}
	if v := rb.DailyCostUSD; v != nil {
		f, ok := budgetDecimal(v)
		if !ok || f <= 0 {
			return fail("budget", "daily_cost_usd", "must be a number of dollars greater than 0 (got %s)", describeTOMLValue(v))
		}
		db.DailyCostUSD = f
	}

	// Sorted, so the first error a file with several bad prices reports is
	// the same one every time.
	models := make([]string, 0, len(rb.Prices))
	for m := range rb.Prices {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, model := range models {
		table := "budget.prices." + model
		// A price is matched against a served model name, which carries no
		// whitespace (registerHarness's `model` rule), so a name that does
		// could never be used.
		if strings.TrimSpace(model) == "" || strings.ContainsFunc(model, unicode.IsSpace) {
			return core.DaemonBudget{}, newError(filename, keyLine(table, ""),
				"[budget.prices.%q]: a model name must be non-blank and carry no whitespace", model)
		}
		rp := rb.Prices[model]
		var p core.Price
		for _, f := range []struct {
			key      string
			v        any
			required bool
			dst      *float64
		}{
			{"input_per_mtok", rp.InputPerMTok, true, &p.InputPerMTok},
			{"output_per_mtok", rp.OutputPerMTok, true, &p.OutputPerMTok},
			{"cache_write_per_mtok", rp.CacheWritePerMTok, false, &p.CacheWritePerMTok},
			{"cache_read_per_mtok", rp.CacheReadPerMTok, false, &p.CacheReadPerMTok},
		} {
			if f.v == nil {
				if f.required {
					// REQ-2 "A price with a missing required field": the
					// model and the field, at the price's own table.
					return core.DaemonBudget{}, newError(filename, keyLine(table, ""),
						"[budget.prices.%q]: missing required %q (dollars per million tokens; input_per_mtok and output_per_mtok are both required)",
						model, f.key)
				}
				continue
			}
			v, ok := budgetDecimal(f.v)
			if !ok || v < 0 {
				return fail(table, f.key, "must be a number of dollars per million tokens, 0 or more (got %s)", describeTOMLValue(f.v))
			}
			*f.dst = v
		}
		if db.Prices == nil {
			db.Prices = map[string]core.Price{}
		}
		db.Prices[model] = p
	}

	groups := make([]string, 0, len(rb.Group))
	for g := range rb.Group {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, group := range groups {
		table := "budget.group." + group
		if !core.ValidQuotaGroup(group) {
			return core.DaemonBudget{}, newError(filename, keyLine(table, ""),
				"[budget.group.%q]: a group name is a quota_group, 1 to 64 of a-z, 0-9, \".\", \"_\" and \"-\"", group)
		}
		v := rb.Group[group].MaxConcurrent
		if v == nil {
			// The table's only key: without it the group caps nothing.
			return core.DaemonBudget{}, newError(filename, keyLine(table, ""),
				"[budget.group.%q]: missing required \"max_concurrent\" (a group table with no cap does nothing)", group)
		}
		n, ok := budgetWhole(v)
		if !ok || n < 1 {
			return fail(table, "max_concurrent", "must be a whole number of at least 1 (got %s)", describeTOMLValue(v))
		}
		if db.Groups == nil {
			db.Groups = map[string]core.GroupBudget{}
		}
		db.Groups[group] = core.GroupBudget{MaxConcurrent: n}
	}
	return db, nil
}

// budgetTableLabel renders a [budget...] table path for an error, quoting a
// price's model name or a group's name the way TOML would
// (budget.prices."gpt-5"), since either may carry a ".".
func budgetTableLabel(table string) string {
	for _, prefix := range []string{"budget.prices.", "budget.group."} {
		if name, ok := strings.CutPrefix(table, prefix); ok {
			return prefix + strconv.Quote(name)
		}
	}
	return table
}

// budgetTableLine is the line errors about [budget] fall back to: its own
// header, or the first [budget.*] header when only sub-tables are written.
func budgetTableLine(headers []tableHeader) int {
	if l := lineOf(headers, "budget"); l > 0 {
		return l
	}
	for _, h := range headers {
		if len(h.parts) > 0 && h.parts[0] == "budget" {
			return h.line
		}
	}
	return 0
}

// budgetWhole reads v as a TOML integer that fits an int.
func budgetWhole(v any) (int, bool) {
	n, ok := v.(int64)
	if !ok || int64(int(n)) != n {
		return 0, false
	}
	return int(n), true
}

// budgetDecimal reads v as a finite TOML number. An integer is accepted: an
// operator who writes `max_cost_usd = 2` means two dollars. TOML's inf and
// nan are refused, since neither is an amount of money.
func budgetDecimal(v any) (float64, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// budgetDuration reads v as a Go duration string such as "15m".
func budgetDuration(v any) (time.Duration, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	d, err := time.ParseDuration(strings.TrimSpace(s))
	return d, err == nil
}

// describeTOMLValue renders a decoded TOML value for an error message: what
// the operator wrote, as near as the decoder lets us say it.
func describeTOMLValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "nothing"
	case string:
		return strconv.Quote(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		return "an array"
	case map[string]any:
		return "a table"
	case time.Time:
		return "a date-time"
	default:
		return fmt.Sprintf("a %T", v)
	}
}
