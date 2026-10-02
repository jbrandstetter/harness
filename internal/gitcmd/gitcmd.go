// gitcmd is the one shared git subprocess runner. skillserve carried two
// private copies and agent packages need a third; a fourth and fifth copy
// would drift apart exactly where it matters most — error output that has to
// be redacted before it reaches an operator.
//
// Governing: ADR-0044 (agent package stables; the CLI tree owns every git
// action), SPEC-0026 REQ-1 (stable update is the only fetcher).
//
// @joestump-agent 10/02/2026 - Extracted from skillserve for harness#811.
package gitcmd

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Run runs git with args in dir ("" for the process working directory) and
// returns its combined output verbatim. The caller decides how much of that
// output belongs in an error, and must redact it before printing a remote
// URL back (git embeds the remote in its own messages).
func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// Raw runs git with args in dir and returns stdout as raw bytes, with
// stderr carried on the error. Binary outputs (git archive) use this.
func Raw(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, fmt.Errorf("git %s: %w", args[0], err)
		}
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.Bytes(), nil
}

// Output runs git with args in dir and returns trimmed stdout only, with
// stderr carried on the error. Read-only commands use this so a warning on
// stderr cannot pollute a parsed ref.
func Output(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}
