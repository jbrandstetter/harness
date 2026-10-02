package main

// Doctor: Agent Package Pins
//
// A warn row for a harness whose source pin is missing from local disk
// (SPEC-0026 REQ-12), distinct from an ordinary load error. The row reads
// the RUNNING daemon's harness records — its last-good config view — so it
// catches exactly the case the fresh config load cannot show: the daemon
// started when the pin existed, and it was pruned or deleted since. A
// missing pin that fails the config load itself is reported by the config
// row as a load failure, not as this row.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-12.
//
// @joestump-agent 10/02/2026 - Added for harness#815.

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

// agentsListClient is the daemon call the check needs, so tests can stub it.
type agentsListClient interface {
	List() ([]protocol.HarnessInfo, error)
}

// agentPinsCheck walks the running daemon's harness records and warns for
// every package-sourced harness whose pin directory is gone from local
// disk. It returns nil when the daemon lists no package-sourced harness.
func agentPinsCheck(c agentsListClient) *check {
	hs, err := c.List()
	if err != nil {
		// The daemon row already reports the failure; this row has nothing
		// of its own to say.
		return nil
	}
	var missing []string
	for _, h := range hs {
		if h.Source == "" {
			continue
		}
		src, err := agentpkg.ParseSource(h.Source)
		if err != nil {
			continue
		}
		if _, err := agentpkg.PinStat(src); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", h.Name, h.Source))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	detail := fmt.Sprintf("%d harness pin(s) missing from local disk: %s", len(missing), strings.Join(missing, ", "))
	return &check{
		name:   "agent_pins",
		level:  cliui.LevelWarn,
		detail: detail,
		hint:   "re-install with `harness agent install <stable>/<package>@<sha>` (the daemon keeps serving its last-good config until then)",
	}
}
