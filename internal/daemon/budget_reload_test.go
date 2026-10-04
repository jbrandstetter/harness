package daemon

// Governing: SPEC-0021 REQ-1 scenario "Out-of-range values" ("the daemon keeps
// running on its previous config if this was a reload"); ADR-0006 (a parse
// error keeps the last-good config). Exercised over the real socket, like
// TestReloadKeepsLastGood, and asserted on the budget the daemon still holds,
// not only on the harness still being listed: a reload that half-applied a
// config would keep the harness and lose the cap.
//
// @joestump 10/04/2026 - Added for #465.

import (
	"os"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

const budgetedTOML = `
[budget]
max_concurrent = 2

[harness.sweep]
harness = "claude-code"
prompt = "sweep the fleet"
schedule = "0 7 * * *"
max_runs_per_day = 5
`

func TestReloadKeepsLastGoodOnBadBudget(t *testing.T) {
	td := newTestDaemon(t, budgetedTOML)
	c := td.dial(t, nil)

	for _, tc := range []struct{ bad, key string }{
		{"max_runs_per_day = 0", "max_runs_per_day"},
		{`quota_backoff_max = "48h"`, "quota_backoff_max"},
		{"max_cost_usd = -1", "max_cost_usd"},
		{"daily_cost_usd = -1", "daily_cost_usd"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			bad := strings.Replace(budgetedTOML, "max_runs_per_day = 5", tc.bad, 1)
			if err := os.WriteFile(td.configPath, []byte(bad), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := c.Reload()
			em, ok := err.(*protocol.ErrorMsg)
			if !ok || em.Code != protocol.ErrReload {
				t.Fatalf("reload error = %v, want reload_failed", err)
			}
			// The located error reaches the operator: the harness, the key,
			// and the key's own line (9), not the table header's (5).
			if !strings.Contains(em.Message, `harness "sweep": "`+tc.key+`"`) || !strings.Contains(em.Message, ":9:") {
				t.Errorf("reload error %q does not name %q at line 9", em.Message, tc.key)
			}

			cfg := td.mgr.Config()
			h, ok := cfg.Harnesses["sweep"]
			if !ok {
				t.Fatal("last-good config lost: sweep is gone")
			}
			if h.Budget.MaxRunsPerDay != 5 || cfg.Budget.MaxConcurrent != 2 {
				t.Errorf("last-good budget lost: harness %+v, daemon %+v", h.Budget, cfg.Budget)
			}
		})
	}
}
