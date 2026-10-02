package main

// The interactive half of the SPEC-0026 confirmation gate: the retype prompt
// and the gate runner that turns a Decision into an error. The reader is
// used only when stdin is a TTY — an unattended session can never type the
// confirmation, and the decision function refuses instead of hanging.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-4 (capability
// requests), REQ-5 (content scan and severity).
//
// @joestump-agent 10/02/2026 - Added for harness#812.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/cliui"
)

// readRetype prompts for and reads one line, the operator's retype of
// <stable>/<package>. It must only be called when stdin is a TTY; the check
// here fails loudly rather than hanging on a pipe.
func readRetype(w io.Writer, ref string) (string, error) {
	if !cliui.IsTTY(os.Stdin) {
		return "", errors.New("agent: cannot prompt for a retype on a non-interactive stdin")
	}
	fmt.Fprintf(w, "agent: retype %s to continue: ", ref)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("agent: read retype: %w", err)
	}
	return strings.TrimSpace(line), nil
}

// runGate drives the confirmation decision to a terminal outcome. A Retype
// outcome prompts once — only ever on an interactive stdin — and decides
// again with the typed text; a Refuse returns as an error, wrapped in
// agentpkg.ErrBlockedFinding when a high-severity finding was the reason.
// #813's install and #814's upgrade call this after rendering the report.
func runGate(cmd *cobra.Command, in agentpkg.DecisionInput) error {
	d := agentpkg.Decide(in)
	if d.Outcome == agentpkg.Retype {
		typed, err := readRetype(cmd.OutOrStdout(), in.Ref)
		if err != nil {
			return err
		}
		in.Retyped = typed
		d = agentpkg.Decide(in)
	}
	switch d.Outcome {
	case agentpkg.Proceed:
		return nil
	case agentpkg.Confirm:
		// The ordinary y/N prompt lands with the install command (#813);
		// until then an interactive Confirm that reaches here is a wiring
		// bug, not a silent pass.
		return errors.New("agent: confirmation prompt is not wired for this command yet")
	default:
		if d.Blocked {
			return fmt.Errorf("%w: %s", agentpkg.ErrBlockedFinding, d.Reason)
		}
		return errors.New("agent: " + d.Reason)
	}
}
