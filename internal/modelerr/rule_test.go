package modelerr

// Rule Name Tests
//
// Governing tests: SPEC-0021 REQ-13 and REQ-17 — a park stores, and doctor
// shows, the matched classifier rule's name; ADR-0027 "Security and tenancy"
// and ADR-0008 — never the error text. The names come from a fixed vocabulary
// in classify.go, so the property under test is membership in that vocabulary,
// whatever the note says.
//
// @joestump 10/04/2026 - Added for harness#473.

import (
	"strings"
	"testing"
)

// declaredRules is every rule name ClassifyRule can return, read from the
// tables themselves.
func declaredRules() map[string]bool {
	names := map[string]bool{ruleContextWindow: true}
	for _, rl := range commonRules {
		names[commonTable+"/"+rl.name] = true
	}
	for adapter, rules := range adapterRules {
		for _, rl := range rules {
			names[adapter+"/"+rl.name] = true
		}
	}
	return names
}

func TestClassifyRuleNamesTheMatch(t *testing.T) {
	cases := []struct {
		adapter, note string
		class         Class
		rule          string
	}{
		// The design's two park examples (SPEC-0021 design.md "What is persisted").
		{"crush", `Bad Request: litellm.RateLimitError: AnthropicException - {"type":"error"}`, ClassQuota, "crush/litellm.ratelimiterror"},
		{"claude-code", "Claude AI usage limit reached|1789430400", ClassQuota, "claude-code/usage limit reached"},
		// A split rule keeps its class and gets its own name.
		{"crush", "litellm.BudgetExceededError: Budget has been exceeded! Current cost: 50.1, Max budget: 50.0", ClassQuota, "crush/litellm.budgetexceedederror"},
		{"claude-code", "Credit balance is too low", ClassQuota, "claude-code/credit balance is too low"},
		{"codex", "exceeded retry limit, last status: 429 Too Many Requests", ClassQuota, "codex/rate limit retries exhausted"},
		// The GH #15 / #782 note, in agent-trace's "<code> (<status>): <text>"
		// shape: the shared 429 rule, under the common table.
		{"claude-code", "rate_limit (429): You've hit your session limit · resets 8:20pm (America/Los_Angeles)", ClassQuota, "common/rate limit"},
		// A spent credit balance at a gateway: HTTP 402.
		{"crush", `Payment Required: {"error":{"message":"Insufficient credits. Add more using https://openrouter.ai/settings/credits","code":402}}`, ClassQuota, "common/payment required"},
		{"claude-code", "rate_limit (429): API Error: Server is temporarily limiting requests (not your usage limit)", ClassTransport, "claude-code/not your usage limit"},
		{"generic", "Unauthorized: invalid x-api-key", ClassAuth, "common/unauthorized"},
		// Context window: recognised through the session guard, not a table.
		{"crush", "Bad Request: prompt is too long: 213000 tokens > 200000 maximum", ClassOther, "common/context window"},
	}
	for _, c := range cases {
		class, rule, known := ClassifyRule(c.adapter, c.note)
		if class != c.class || rule != c.rule || !known {
			t.Errorf("ClassifyRule(%q, %q) = (%s, %q, %v), want (%s, %q, true)", c.adapter, c.note, class, rule, known, c.class, c.rule)
		}
	}
}

func TestClassifyRuleUnclassifiedHasNoName(t *testing.T) {
	class, rule, known := ClassifyRule("crush", "Bad Request: tool_use ids must be unique")
	if class != ClassOther || rule != "" || known {
		t.Errorf("unclassified = (%s, %q, %v), want (other, \"\", false)", class, rule, known)
	}
}

// The rule is a label from classify.go, never text from the note: it is in the
// declared vocabulary for every note, including ones built to smuggle text in.
func TestClassifyRuleIsNeverTheErrorText(t *testing.T) {
	declared := declaredRules()
	marker := "NOTE-TEXT-7f3a"
	for _, adapter := range []string{"crush", "claude-code", "codex", "generic", "crush/../" + marker} {
		for _, note := range []string{
			"429 " + marker,
			"rate limit " + marker,
			"usage limit reached|" + marker,
			"dial tcp " + marker + " i/o timeout",
			"litellm.RateLimitError " + marker,
			marker,
		} {
			_, rule, known := ClassifyRule(adapter, note)
			if known && !declared[rule] {
				t.Errorf("ClassifyRule(%q, %q) rule %q is not a declared rule", adapter, note, rule)
			}
			if strings.Contains(rule, marker) {
				t.Errorf("ClassifyRule(%q, %q) rule %q carries note text", adapter, note, rule)
			}
		}
	}
}

// Every name is unique, so a stored rule identifies one rule, and has no "/",
// so "<table>/<name>" splits one way.
func TestRuleNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	check := func(table string, rules []rule) {
		for _, rl := range rules {
			if rl.name == "" || strings.Contains(rl.name, "/") {
				t.Errorf("%s rule %q: a name must be non-empty and free of /", table, rl.name)
			}
			full := table + "/" + rl.name
			if seen[full] {
				t.Errorf("rule %q is declared twice", full)
			}
			seen[full] = true
		}
	}
	check(commonTable, commonRules)
	for adapter, rules := range adapterRules {
		check(adapter, rules)
	}
	if seen[ruleContextWindow] {
		t.Errorf("%q collides with a table rule", ruleContextWindow)
	}
}
