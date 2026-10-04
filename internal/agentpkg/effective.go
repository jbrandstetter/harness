// The upgrade review's effective diff: which manifest-suppliable keys and
// requested scopes actually differ between a harness's current effective
// values and the upgrade candidate (issue #882). A diff is review-worthy
// only when it changes behavior: a manifest-supplied key whose value moves,
// a local override the new pin contradicts, or a requested mcp_allow scope
// the table does not already grant. Package metadata (version,
// description) never triggers review — it changes nothing the harness runs.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-8 (upgrade),
// REQ-4 (capability requests).
//
// @joestump-agent 10/02/2026 - Added for harness#882.
package agentpkg

import (
	"fmt"
	"sort"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// EffectiveChange is one reviewable difference. Kind is "value" for a
// manifest-suppliable key or "request" for an mcp_allow grant.
type EffectiveChange struct {
	// Key is the TOML key on the harness table ("model", "mcp_allow", …).
	Key string
	// Kind is "value" or "request".
	Kind string
	// Old is the current effective value (nil = unset), rendered by Render.
	Old any
	// New is the candidate's value (nil = the new pin drops it).
	New any
	// OldLocal is true when Old came from the operator's table rather than
	// the old pin — the case where "take the new value" must REMOVE the
	// local override for the choice to be honest.
	OldLocal bool
	// Added is true when Old is nil and nothing local can preserve it: the
	// new pin introduces the key, and the only choice is to accept it.
	Added bool
}

// Render formats a value for the review line: lists bracketed, unset as
// "unset".
func (c EffectiveChange) Render(v any) string {
	if v == nil {
		return "unset"
	}
	if l, ok := v.([]string); ok {
		return "[" + strings.Join(l, ", ") + "]"
	}
	return fmt.Sprintf("%v", v)
}

// EffectiveChanges diffs the harness's current effective values against the
// candidate manifest. Provenance comes from h.PackageKeys: a key it lists
// was the old pin's; any other set key is the operator's own. Only
// behavioral differences surface — a local override the new pin contradicts
// (the effective value is the operator's either way, but the divergence is
// worth seeing), a package-supplied value the new pin moves or drops (the
// effective value would silently follow), and a requested mcp_allow scope
// the table does not grant.
func EffectiveChanges(h *core.Harness, newMan *Manifest) []EffectiveChange {
	var out []EffectiveChange
	packageKey := func(key string) bool {
		for _, k := range h.PackageKeys {
			if k == key {
				return true
			}
		}
		return false
	}

	type pair struct {
		key    string
		old    any
		new    any
		oldSet bool
	}
	var pairs []pair
	add := func(key string, old any, oldSet bool, new any) {
		pairs = append(pairs, pair{key, old, new, oldSet})
	}

	hv := newMan.Harness
	add("harness", strOrNil(h.Adapter), h.Adapter != "", strOrNil(hv.Harness))
	add("args", strSliceOrNil(h.Args), h.Args != nil, strSliceOrNil(hv.Args))
	add("argv", strSliceOrNil(h.Argv), h.Argv != nil, strSliceOrNil(hv.Argv))
	add("model", strOrNil(h.Model), h.Model != "", strOrNil(hv.Model))
	// core.Harness flattens the scalar keys, so presence is inferred: a
	// package-supplied value rides PackageKeys, and a non-zero local value
	// is visible. An explicit local false is indistinguishable from unset
	// here — the review handles that case by writing the chosen value
	// explicitly rather than letting it flow.
	add("auto_accept", h.AutoAccept, h.AutoAccept || packageKey("auto_accept"), boolOrNil(hv.AutoAccept))
	add("max_turns", h.MaxTurns, h.MaxTurns != 0 || packageKey("max_turns"), intOrNil(hv.MaxTurns))
	add("quiet", h.Quiet, h.Quiet || packageKey("quiet"), boolOrNil(hv.Quiet))
	add("system_prompt_file", strOrNil(h.SystemPromptFile), h.SystemPromptFile != "", strOrNil(hv.SystemPromptFile))
	add("mcp_config", strOrNil(h.MCPConfig), h.MCPConfig != "", strOrNil(hv.MCPConfig))
	add("allowed_tools", strSliceOrNil(h.AllowedTools), h.AllowedTools != nil, strSliceOrNil(hv.AllowedTools))

	for _, p := range pairs {
		oldLocal := p.oldSet && !packageKey(p.key)
		// A key the new pin does not supply: a package-supplied value being
		// dropped moves the effective value to unset (review); a local one
		// simply stays — nothing the upgrade does touches it.
		if p.new == nil {
			if p.oldSet && !oldLocal {
				out = append(out, EffectiveChange{
					Key:  p.key,
					Kind: "value",
					Old:  p.old,
					New:  nil,
				})
			}
			continue
		}
		if p.oldSet && fmt.Sprint(p.old) == fmt.Sprint(p.new) {
			continue
		}
		switch {
		case !p.oldSet:
			// The new pin introduces the key: the only choice is to accept
			// it — there is nothing local to preserve.
			out = append(out, EffectiveChange{Key: p.key, Kind: "value", New: p.new, Added: true})
		case oldLocal:
			// A local override the new pin now contradicts: the effective
			// value is the operator's either way, but the divergence is
			// exactly what the review exists to surface — taking the new
			// value removes the override.
			out = append(out, EffectiveChange{Key: p.key, Kind: "value", Old: p.old, New: p.new, OldLocal: true})
		default:
			// The old pin supplied it and the new pin moves it: the
			// effective value silently follows the pin unless the operator
			// pins the old one. The auto-accept danger the review guards.
			out = append(out, EffectiveChange{Key: p.key, Kind: "value", Old: p.old, New: p.new})
		}
	}

	// The requested-scope row: every scope the new pin requests that the
	// table does not already grant. Package metadata never lands here.
	if len(newMan.Requests.MCPAllow) > 0 {
		granted := map[string]bool{}
		for _, s := range h.MCPAllow {
			granted[strings.ToLower(s)] = true
		}
		var missing []string
		for _, s := range newMan.Requests.MCPAllow {
			if !granted[strings.ToLower(s)] {
				missing = append(missing, s)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			out = append(out, EffectiveChange{
				Key:  "mcp_allow",
				Kind: "request",
				Old:  h.MCPAllow,
				New:  newMan.Requests.MCPAllow,
			})
		}
	}
	return out
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strSliceOrNil(s []string) any {
	if s == nil {
		return nil
	}
	return s
}

func boolOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func intOrNil(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}
