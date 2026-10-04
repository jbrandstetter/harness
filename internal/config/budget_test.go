package config

// Run Budget Config Tests
//
// Governing tests: SPEC-0021 REQ-1 "Per-harness budget keys" (every scenario:
// "A valid budgeted one-shot", "A per-run cap on a resident harness", "Budget
// keys in a project file", "Out-of-range values") and REQ-2 "The [budget]
// table" ("A budget day in another zone" at the parse level, "An unknown
// zone", "A price with a missing required field"). Every refusal is asserted
// on its line as well as its words, because a located error is the
// requirement, and a check that only matched the message would pass with the
// header's line too.
//
// @joestump 10/04/2026 - Added for #465.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// wantConfigErr asserts err is a located *Error on line, whose message
// contains every one of subs.
func wantConfigErr(t *testing.T, err error, line int, subs ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("loaded without error; want a refusal on line %d containing %q", line, subs)
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error %v (%T) is not a located *config.Error", err, err)
	}
	if ce.Line != line {
		t.Errorf("error on line %d, want line %d: %v", ce.Line, line, err)
	}
	for _, s := range subs {
		if !strings.Contains(ce.Msg, s) {
			t.Errorf("error %q does not contain %q", ce.Msg, s)
		}
	}
}

// TestBudgetValidOneShot is REQ-1 "A valid budgeted one-shot", with every
// other per-harness key set too, so one fixture pins the whole parse.
func TestBudgetValidOneShot(t *testing.T) {
	src := fmt.Sprintf(`
[harness.pr-review]
harness = "claude-code"
prompt_file = %q
schedule = "*/10 * * * *"
max_runs_per_day = 40
max_tokens = 400000
max_cost_usd = 2.0
daily_cost_usd = 20
quota_group = "claude-max"
quota_backoff = "10m"
quota_backoff_max = "4h"
`, writePromptFile(t, "review the open pull requests\n"))
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatalf("budgeted one-shot did not load: %v", err)
	}
	got := cfg.Harnesses["pr-review"].Budget
	want := core.Budget{
		MaxRunsPerDay:   40,
		MaxTokens:       400000,
		MaxCostUSD:      2.0,
		DailyCostUSD:    20, // an integer is a whole number of dollars
		QuotaGroup:      "claude-max",
		QuotaBackoff:    10 * time.Minute,
		QuotaBackoffMax: 4 * time.Hour,
	}
	if got != want {
		t.Errorf("Budget = %+v\nwant     %+v", got, want)
	}
}

// TestBudgetResidentKeys pins which keys a resident harness may carry: every
// one but the per-run caps (ADR-0027 § "The schema").
func TestBudgetResidentKeys(t *testing.T) {
	src := `
[harness.crush-sb]
harness = "crush"
args = ["--yolo"]
enabled = true
max_runs_per_day = 30
daily_cost_usd = 15.5
quota_group = "litellm"
quota_backoff = "15m"
quota_backoff_max = "6h"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatalf("budgeted resident did not load: %v", err)
	}
	b := cfg.Harnesses["crush-sb"].Budget
	if b.MaxRunsPerDay != 30 || b.DailyCostUSD != 15.5 || b.QuotaGroup != "litellm" ||
		b.QuotaBackoff != 15*time.Minute || b.QuotaBackoffMax != 6*time.Hour {
		t.Errorf("Budget = %+v", b)
	}
}

// TestBudgetAbsentIsZero is REQ-1's "a harness with none of these keys SHALL
// behave exactly as before": the zero Budget, whose backoffs still resolve to
// the documented defaults, since quota parking applies to every harness.
func TestBudgetAbsentIsZero(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Harnesses["a"].Budget
	if b != (core.Budget{}) {
		t.Errorf("Budget = %+v, want the zero value", b)
	}
	if b.EffectiveQuotaBackoff() != 15*time.Minute || b.EffectiveQuotaBackoffMax() != 6*time.Hour {
		t.Errorf("effective backoff = %v / %v, want 15m / 6h", b.EffectiveQuotaBackoff(), b.EffectiveQuotaBackoffMax())
	}
	if cfg.Budget.MaxConcurrent != 0 || cfg.Budget.DailyCostUSD != 0 || cfg.Budget.Prices != nil || cfg.Budget.Groups != nil {
		t.Errorf("DaemonBudget = %+v, want the zero value", cfg.Budget)
	}
	if cfg.Budget.DayStarts.Location() != time.Local || cfg.Budget.DayStarts.String() != "00:00" {
		t.Errorf("default day_starts = %s in %v, want 00:00 in the daemon's zone", cfg.Budget.DayStarts, cfg.Budget.DayStarts.Location())
	}
}

// TestBudgetPerRunCapOnResident is REQ-1 "A per-run cap on a resident
// harness": the error names the harness, the key and the key's line.
func TestBudgetPerRunCapOnResident(t *testing.T) {
	for _, key := range []string{"max_tokens = 100000", "max_cost_usd = 1.5"} {
		src := "[harness.crush-sb]\nharness = \"crush\"\nenabled = true\n" + key + "\n"
		_, err := Parse([]byte(src), "t.toml")
		wantConfigErr(t, err, 4, `"crush-sb"`, `"`+strings.Fields(key)[0]+`"`, "per-run caps apply only to one-shot harnesses")
	}
}

// TestBudgetPerRunCapOnCommandOneShot pins the spec's literal predicate: a
// per-run cap needs prompt or prompt_file, and a scheduled command harness
// has neither, so it is refused like a resident.
func TestBudgetPerRunCapOnCommandOneShot(t *testing.T) {
	src := `[harness.report]
harness = "command"
argv = ["/usr/local/bin/report"]
schedule = "0 7 * * *"
max_tokens = 1000
`
	_, err := Parse([]byte(src), "t.toml")
	wantConfigErr(t, err, 5, `"report"`, `"max_tokens"`, "per-run caps apply only to one-shot harnesses")
}

// TestBudgetOutOfRange is REQ-1 "Out-of-range values", plus every wrong type
// and every bound the REQ-1 table states. Each fixture puts the bad key on
// line 4, so the assertion proves the error is located at the key and not at
// the [harness.x] header on line 1.
func TestBudgetOutOfRange(t *testing.T) {
	cases := []struct {
		key  string // the bad assignment, written on line 4
		want string
	}{
		// The three values REQ-1's scenario names.
		{`max_runs_per_day = 0`, "must be a whole number of at least 1 (got 0)"},
		{`quota_backoff_max = "48h"`, `must be a duration no longer than 24h, such as "6h" (got "48h")`},
		{`max_cost_usd = -1`, "must be a number of dollars greater than 0 (got -1)"},

		{`max_runs_per_day = -3`, "at least 1"},
		{`max_runs_per_day = 2.5`, "must be a whole number of at least 1 (got 2.5)"},
		{`max_runs_per_day = "40"`, `must be a whole number of at least 1 (got "40")`},
		{`max_runs_per_day = true`, "(got true)"},
		{`max_tokens = 0`, "must be a whole number of at least 1 (got 0)"},
		{`max_tokens = 1e6`, "(got 1e+06)"},
		{`max_cost_usd = 0`, "greater than 0 (got 0)"},
		{`max_cost_usd = "2.00"`, `(got "2.00")`},
		{`max_cost_usd = nan`, "(got NaN)"},
		{`max_cost_usd = inf`, "(got +Inf)"},
		{`daily_cost_usd = 0.0`, "greater than 0 (got 0)"},
		{`daily_cost_usd = [5]`, "(got an array)"},
		{`quota_group = "Claude Max"`, `must be 1 to 64 of a-z, 0-9, ".", "_" and "-" (got "Claude Max")`},
		{`quota_group = ""`, `(got "")`},
		{`quota_group = "` + strings.Repeat("a", 65) + `"`, "1 to 64"},
		{`quota_group = 7`, "(got 7)"},
		{`quota_backoff = "30s"`, `must be a duration of at least 1m, such as "15m" (got "30s")`},
		{`quota_backoff = "15"`, `(got "15")`},
		{`quota_backoff = 15`, "(got 15)"},
		{`quota_backoff_max = "0s"`, "no longer than 24h"},
		{`quota_backoff_max = "-1h"`, "no longer than 24h"},
		// The ordering rule, on effective values: 8h is past the 6h default
		// ceiling, and 5m is under the 15m default first park.
		{`quota_backoff = "8h"`, "must not exceed quota_backoff_max, which defaults to 6h"},
		{`quota_backoff_max = "5m"`, "must be at least quota_backoff, which defaults to 15m"},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			src := fmt.Sprintf("[harness.pr-review]\nharness = \"claude-code\"\nprompt = \"review\"\n%s\n", c.key)
			_, err := Parse([]byte(src), "t.toml")
			key := strings.TrimSpace(strings.SplitN(c.key, "=", 2)[0])
			wantConfigErr(t, err, 4, `"pr-review"`, `"`+key+`"`, c.want)
		})
	}
}

// TestBudgetBackoffMaxBelowBackoff blames quota_backoff_max, the key REQ-1
// states the ordering on, when both are set.
func TestBudgetBackoffMaxBelowBackoff(t *testing.T) {
	src := "[harness.a]\nharness = \"crush\"\nquota_backoff = \"2h\"\nquota_backoff_max = \"1h\"\n"
	_, err := Parse([]byte(src), "t.toml")
	wantConfigErr(t, err, 4, `"quota_backoff_max"`, `must be at least quota_backoff ("2h") (got "1h")`)
}

// TestBudgetKeysInProjectFile is REQ-1 "Budget keys in a project file": every
// key is refused, naming the key and saying budgets are global-only, at the
// key's line.
func TestBudgetKeysInProjectFile(t *testing.T) {
	for _, key := range []string{
		`max_runs_per_day = 5`, `max_tokens = 1000`, `max_cost_usd = 1.0`, `daily_cost_usd = 5.0`,
		`quota_group = "claude-max"`, `quota_backoff = "15m"`, `quota_backoff_max = "6h"`,
	} {
		name := strings.TrimSpace(strings.SplitN(key, "=", 2)[0])
		src := "[harness.agent]\nharness = \"claude-code\"\nprompt = \"go\"\n" + key + "\n"
		_, err := ParseProject([]byte(src), "/tmp/repo/harness.toml")
		wantConfigErr(t, err, 4, `"agent"`, `"`+name+`"`, "not accepted in a project file", "only accepted in the global config")
	}
	// A bare [name] table is a project harness too.
	_, err := ParseProject([]byte("[agent]\nharness = \"crush\"\n\nmax_runs_per_day = 5\n"), "/tmp/repo/harness.toml")
	wantConfigErr(t, err, 4, `"max_runs_per_day"`, "not accepted in a project file")
}

// TestBudgetTableInProjectFile is REQ-2's "[budget] in a project file SHALL
// be rejected", whichever header spells it.
func TestBudgetTableInProjectFile(t *testing.T) {
	for _, header := range []string{"[budget]", `[budget.prices."gpt-5"]`, "[budget.group.claude-max]"} {
		src := "[harness.agent]\nharness = \"crush\"\n\n" + header + "\n"
		_, err := ParseProject([]byte(src), "/tmp/repo/harness.toml")
		wantConfigErr(t, err, 4, "project file must not contain [budget", "only accepted in the global config")
	}
}

// TestBudgetInDropIn pins REQ-1's refusal of the keys, and REQ-2's of the
// table, in a harness_d drop-in, located in the drop-in file.
func TestBudgetInDropIn(t *testing.T) {
	cases := []struct {
		name, body string
		line       int
		want       string
	}{
		{"key", "[harness.unit]\nharness = \"crush\"\nmax_runs_per_day = 5\n", 3, `harness "unit": "max_runs_per_day" is not accepted in a harness_d drop-in`},
		{"table", "[budget]\nmax_concurrent = 2\n", 1, "harness.d file must not contain [budget]"},
		{"price", "[budget.prices.\"gpt-5\"]\ninput_per_mtok = 1\noutput_per_mtok = 2\n", 1, "harness.d file must not contain [budget.prices.gpt-5]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dropIn := filepath.Join(dir, "unit.toml")
			if err := os.WriteFile(dropIn, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Parse([]byte(fmt.Sprintf("[server]\nharness_d = %q\n", dir)), "t.toml")
			wantConfigErr(t, err, c.line, c.want)
			var ce *Error
			if errors.As(err, &ce) && ce.File != dropIn {
				t.Errorf("error located in %q, want the drop-in %q", ce.File, dropIn)
			}
		})
	}
}

// TestDaemonBudgetParses covers the whole [budget] table, including prices
// spelled every way TOML allows and a group no harness names (REQ-2: it
// loads; doctor warns, in #486).
func TestDaemonBudgetParses(t *testing.T) {
	src := `
[budget]
day_starts = "TZ=America/Los_Angeles 06:00"
max_concurrent = 4
daily_cost_usd = 50.00
prices.dotted-model.input_per_mtok = 0.5
prices.dotted-model.output_per_mtok = 1.5

[budget.prices."claude-sonnet-4-6"]
input_per_mtok = 3.00
output_per_mtok = 15.00
cache_write_per_mtok = 3.75
cache_read_per_mtok = 0.30

[budget.prices."openrouter/z-ai/glm-5.3"]
input_per_mtok = 1
output_per_mtok = 2

[budget.group.claude-max]
max_concurrent = 2

[harness.a]
harness = "crush"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatalf("[budget] did not load: %v", err)
	}
	if got := cfg.HarnessOrder; len(got) != 1 || got[0] != "a" {
		t.Errorf("HarnessOrder = %v: [budget] must not register as a bare harness", got)
	}
	db := cfg.Budget
	if db.DayStarts.String() != "TZ=America/Los_Angeles 06:00" {
		t.Errorf("DayStarts = %s", db.DayStarts)
	}
	if db.MaxConcurrent != 4 || db.DailyCostUSD != 50 {
		t.Errorf("MaxConcurrent/DailyCostUSD = %d/%v", db.MaxConcurrent, db.DailyCostUSD)
	}
	wantPrices := map[string]core.Price{
		"claude-sonnet-4-6":       {InputPerMTok: 3, OutputPerMTok: 15, CacheWritePerMTok: 3.75, CacheReadPerMTok: 0.30},
		"openrouter/z-ai/glm-5.3": {InputPerMTok: 1, OutputPerMTok: 2},
		"dotted-model":            {InputPerMTok: 0.5, OutputPerMTok: 1.5},
	}
	if len(db.Prices) != len(wantPrices) {
		t.Errorf("Prices = %+v, want %+v", db.Prices, wantPrices)
	}
	for m, want := range wantPrices {
		if got := db.Prices[m]; got != want {
			t.Errorf("Prices[%q] = %+v, want %+v", m, got, want)
		}
	}
	if g, ok := db.Groups["claude-max"]; !ok || g.MaxConcurrent != 2 {
		t.Errorf("Groups = %+v, want claude-max with max_concurrent 2", db.Groups)
	}
}

// TestDaemonBudgetSubTablesOnly: a file may write [budget.prices.*] with no
// [budget] header at all, and it is still the one table.
func TestDaemonBudgetSubTablesOnly(t *testing.T) {
	src := "[budget.prices.\"gpt-5\"]\ninput_per_mtok = 1.25\noutput_per_mtok = 10\n"
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Budget.Prices["gpt-5"]; got.InputPerMTok != 1.25 || got.OutputPerMTok != 10 {
		t.Errorf("Prices[gpt-5] = %+v", got)
	}
}

// TestDaemonBudgetUnknownZone is REQ-2 "An unknown zone": SPEC-0008's
// unknown-zone error, located at the key.
func TestDaemonBudgetUnknownZone(t *testing.T) {
	src := "[budget]\nmax_concurrent = 2\nday_starts = \"TZ=Mars/Olympus 00:00\"\n"
	_, err := Parse([]byte(src), "t.toml")
	wantConfigErr(t, err, 3, `"day_starts"`, `unknown time zone "Mars/Olympus"`)
}

// TestDaemonBudgetPriceMissingField is REQ-2 "A price with a missing required
// field": the load fails naming the model and the field.
func TestDaemonBudgetPriceMissingField(t *testing.T) {
	src := "[budget]\nmax_concurrent = 2\n\n[budget.prices.\"gpt-5\"]\noutput_per_mtok = 10\n"
	_, err := Parse([]byte(src), "t.toml")
	wantConfigErr(t, err, 4, `[budget.prices."gpt-5"]`, `missing required "input_per_mtok"`)

	src = "[budget.prices.\"gpt-5\"]\ninput_per_mtok = 10\n"
	_, err = Parse([]byte(src), "t.toml")
	wantConfigErr(t, err, 1, `[budget.prices."gpt-5"]`, `missing required "output_per_mtok"`)
}

// TestDaemonBudgetRejects covers every other [budget] refusal, each at its
// key's line.
func TestDaemonBudgetRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
		want []string
	}{
		{"max_concurrent zero", "[budget]\nmax_concurrent = 0\n", 2, []string{`[budget] "max_concurrent"`, "at least 1 (got 0)"}},
		{"max_concurrent float", "[budget]\nmax_concurrent = 1.5\n", 2, []string{`"max_concurrent"`, "(got 1.5)"}},
		{"daily_cost_usd negative", "[budget]\ndaily_cost_usd = -5\n", 2, []string{`[budget] "daily_cost_usd"`, "greater than 0 (got -5)"}},
		{"day_starts not a string", "[budget]\nday_starts = 6\n", 2, []string{`[budget] "day_starts"`, "must be a string"}},
		{"day_starts 24:00", "[budget]\nday_starts = \"24:00\"\n", 2, []string{`"day_starts"`, "cannot start at 24:00"}},
		{"day_starts blank", "[budget]\nday_starts = \" \"\n", 2, []string{`"day_starts"`, "must not be blank"}},
		{"day_starts a window", "[budget]\nday_starts = \"Mon 06:00\"\n", 2, []string{`"day_starts"`, "malformed"}},
		{"negative price", "[budget.prices.\"gpt-5\"]\ninput_per_mtok = -1\noutput_per_mtok = 1\n", 2,
			[]string{`[budget.prices."gpt-5"] "input_per_mtok"`, "0 or more (got -1)"}},
		{"string price", "[budget.prices.\"gpt-5\"]\ninput_per_mtok = 1\noutput_per_mtok = \"1\"\n", 3,
			[]string{`"output_per_mtok"`, `(got "1")`}},
		{"bad cache price", "[budget.prices.\"gpt-5\"]\ninput_per_mtok = 1\noutput_per_mtok = 1\ncache_read_per_mtok = -0.1\n", 4,
			[]string{`"cache_read_per_mtok"`, "(got -0.1)"}},
		{"model with a space", "[budget.prices.\"gpt 5\"]\ninput_per_mtok = 1\noutput_per_mtok = 1\n", 1,
			[]string{`[budget.prices."gpt 5"]`, "no whitespace"}},
		{"unknown price key", "[budget.prices.\"gpt-5\"]\ninput_per_mtok = 1\noutput_per_mtok = 1\ninput_price = 1\n", 4,
			[]string{`unknown key "input_price"`}},
		{"unknown budget key", "[budget]\nmax_concurent = 2\n", 2, []string{`unknown key "max_concurent" in [budget]`}},
		{"bad group name", "[budget.group.\"Claude Max\"]\nmax_concurrent = 1\n", 1, []string{`[budget.group."Claude Max"]`, "a group name is a quota_group"}},
		{"group without cap", "[budget.group.claude-max]\n", 1, []string{`[budget.group."claude-max"]`, `missing required "max_concurrent"`}},
		{"group cap zero", "[budget.group.claude-max]\nmax_concurrent = 0\n", 2, []string{`[budget.group."claude-max"] "max_concurrent"`, "at least 1 (got 0)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.src), "t.toml")
			wantConfigErr(t, err, c.line, c.want...)
		})
	}
}
