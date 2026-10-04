package tui

// Governing: SPEC-0021 REQ-1 "Per-harness budget keys", REQ-2 "The [budget]
// table"; SPEC-0001 REQ "Lossless Edit Round-Trip". The config writers must
// not drop a budget: an edit that silently lifted a spend cap would read as a
// healthy harness until the bill arrived. Every assertion is on the re-parsed
// values, not the bytes, so a writer free to reformat stays free to.
//
// @joestump 10/04/2026 - Added for #465.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

// TestEditPreservesBudget edits a budgeted harness's description through the
// `e` save path and checks that the harness's budget keys and the whole
// [budget] table (written both before and after the edited table, since the
// rewrite removes up to the next header) come back unchanged.
func TestEditPreservesBudget(t *testing.T) {
	original := strings.Join([]string{
		"[budget]",
		`day_starts = "TZ=America/Los_Angeles 06:00"`,
		"max_concurrent = 4",
		"daily_cost_usd = 50.0",
		"",
		"[harness.night-owl]",
		`harness = "claude-code"`,
		`args = ["--remote-control"]`,
		"enabled = true",
		`description = "before"`,
		"max_runs_per_day = 30",
		"daily_cost_usd = 15.25",
		`quota_group = "claude-max"`,
		`quota_backoff = "20m"`,
		`quota_backoff_max = "12h"`,
		"",
		`[budget.prices."claude-sonnet-4-6"]`,
		"input_per_mtok = 3.00",
		"output_per_mtok = 15.00",
		"cache_write_per_mtok = 3.75",
		"cache_read_per_mtok = 0.30",
		"",
		"[budget.group.claude-max]",
		"max_concurrent = 2",
		"",
	}, "\n")
	before, err := config.Parse([]byte(original), "harness.toml")
	if err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}

	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	form := editInputsFor(path, protocol.HarnessInfo{Name: "night-owl"}).toForm()
	form.Description = "after"
	if err := form.Validate(); err != nil {
		t.Fatalf("edit failed validation: %v", err)
	}
	body := AppendHarness([]byte(removeHarnessTOML(original, "night-owl")), form)
	after, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("rewritten config no longer parses: %v\n---\n%s", err, body)
	}

	if got := after.Harnesses["night-owl"].Description; got != "after" {
		t.Fatalf("the edit itself did not land: description = %q", got)
	}
	if got, want := after.Harnesses["night-owl"].Budget, before.Harnesses["night-owl"].Budget; got != want {
		t.Errorf("harness budget changed across the edit:\n before: %+v\n  after: %+v\n---\n%s", want, got, body)
	}
	b, a := before.Budget, after.Budget
	if a.DayStarts.String() != b.DayStarts.String() || a.MaxConcurrent != b.MaxConcurrent || a.DailyCostUSD != b.DailyCostUSD {
		t.Errorf("[budget] changed across the edit:\n before: %s %d %v\n  after: %s %d %v",
			b.DayStarts, b.MaxConcurrent, b.DailyCostUSD, a.DayStarts, a.MaxConcurrent, a.DailyCostUSD)
	}
	if len(a.Prices) != 1 || a.Prices["claude-sonnet-4-6"] != b.Prices["claude-sonnet-4-6"] {
		t.Errorf("[budget.prices] changed across the edit: before %+v, after %+v", b.Prices, a.Prices)
	}
	if len(a.Groups) != 1 || a.Groups["claude-max"] != b.Groups["claude-max"] {
		t.Errorf("[budget.group] changed across the edit: before %+v, after %+v", b.Groups, a.Groups)
	}
}

// TestHarnessFormBudgetTOML is the `n` write path: a form carrying every
// budget key serializes to TOML the parser reads back to the same values,
// including an amount 'g' formatting writes in exponent form and a duration
// that is not a whole number of hours.
func TestHarnessFormBudgetTOML(t *testing.T) {
	want := core.Budget{
		MaxRunsPerDay:   7,
		MaxTokens:       1_500_000,
		MaxCostUSD:      1e6,
		DailyCostUSD:    0.125,
		QuotaGroup:      "openrouter.glm_5-3",
		QuotaBackoff:    90 * time.Second,
		QuotaBackoffMax: 90 * time.Minute,
	}
	f := NewHarnessForm()
	f.Name = "sweep"
	f.Harness = "claude-code"
	f.Prompt = "sweep the fleet"
	f.Schedule = "0 7 * * *"
	f.Budget = want
	if err := f.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	cfg, err := config.Parse([]byte(f.TOML()), "harness.toml")
	if err != nil {
		t.Fatalf("form TOML does not parse: %v\n---\n%s", err, f.TOML())
	}
	if got := cfg.Harnesses["sweep"].Budget; got != want {
		t.Errorf("Budget = %+v\nwant     %+v\n---\n%s", got, want, f.TOML())
	}

	// And a harness with no budget writes no budget key at all.
	f.Budget = core.Budget{}
	for _, key := range []string{"max_runs_per_day", "max_tokens", "max_cost_usd", "daily_cost_usd", "quota_group", "quota_backoff"} {
		if strings.Contains(f.TOML(), key) {
			t.Errorf("an unbudgeted form wrote %q:\n%s", key, f.TOML())
		}
	}
}

// TestHarnessFormRefusesPerRunCapWithoutPrompt: the budget rides the form
// unedited, so clearing a capped one-shot's prompt is the one way a save could
// write a table the parser refuses (SPEC-0021 REQ-1 "A per-run cap on a
// resident harness"). Validate mirrors the parser and refuses it first.
func TestHarnessFormRefusesPerRunCapWithoutPrompt(t *testing.T) {
	for _, b := range []core.Budget{{MaxTokens: 100000}, {MaxCostUSD: 2}} {
		f := NewHarnessForm()
		f.Name = "crush-sb"
		f.Harness = "crush"
		f.Enabled = true
		f.Budget = b
		err := f.Validate()
		if err == nil || !strings.Contains(err.Error(), "per-run caps") {
			t.Errorf("Validate(%+v) = %v, want the per-run cap refusal", b, err)
		}
		if _, perr := config.Parse([]byte(f.TOML()), "harness.toml"); perr == nil {
			t.Errorf("the parser accepted what Validate refuses, so the mirror is stale:\n%s", f.TOML())
		}
	}
}
