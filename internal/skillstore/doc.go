// Package skillstore
//
// The learned skill tier's substrate: a Harness-owned, git-tracked markdown
// store under the state directory, one directory per skill, plus the status
// model that gates both delivery channels. A learned skill is markdown, never
// projected into any harness's native skill directory, and reachable only
// through search.
//
// Governing: ADR-0012 (search-only learned tier); SPEC-0007 REQ "Skill
// Artifact", REQ "Two-Channel Gate", REQ "Error Handling Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#77.
package skillstore
