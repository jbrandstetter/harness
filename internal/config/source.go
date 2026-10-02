// source.go resolves a [harness.*] table's `source` reference against the
// content-addressed pin store at config load (SPEC-0026 REQ-7). The daemon's
// one and only step: read package.toml from local disk, apply the package's
// [harness] values, and let any key present directly on the table override
// (the ADR-0011 precedence rule skill_paths already follows). No network
// request, no git operation — a missing pin fails the load and keeps the
// daemon on its last-good configuration, exactly as an unknown adapter does
// (ADR-0006).
//
// Governing: ADR-0044, SPEC-0026 REQ-7, Error Handling Standards.
package config

import (
	"errors"
	"io/fs"
	"path/filepath"
	"sort"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// applySource merges the pinned package's [harness] values into rh and returns
// the merged table. Local keys win on presence: a key set directly on the
// harness table overrides the package's value for that key (SPEC-0026 REQ-7).
// Relative path values from the manifest resolve against the package's own
// pin directory, never the installing harness's workdir (REQ-3).
func applySource(filename, name string, line int, rh rawHarness) (rawHarness, error) {
	src, err := agentpkg.ParseSource(rh.Source)
	if err != nil {
		return rh, newError(filename, line, "harness %q: invalid \"source\": %v", name, err)
	}
	man, err := agentpkg.LoadManifest(agentpkg.ManifestPath(src))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rh, newError(filename, line,
				"harness %q: source %q is not installed on this machine — run `harness agent install %s@%s` (no fetch is performed on your behalf)",
				name, rh.Source, src.Stable+"/"+src.Package, src.SHA)
		}
		return rh, newError(filename, line, "harness %q: source %q: %v", name, rh.Source, err)
	}
	if man.Package.Name != src.Package {
		return rh, newError(filename, line,
			"harness %q: source %q names package %q but its manifest declares %q",
			name, rh.Source, src.Package, man.Package.Name)
	}

	hv := man.Harness
	pinDir := agentpkg.PinDir(src)
	// Manifest-relative paths anchor on the pin directory; already-absolute
	// paths pass through untouched.
	manifestPath := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(pinDir, p)
	}

	// Every key filled below is the package's, not the operator's: record
	// it so describe can attribute the effective value (SPEC-0026 REQ-12).
	fromPackage := func(key string) {
		rh.PackageKeys = append(rh.PackageKeys, key)
	}
	if rh.Harness == "" && hv.Harness != "" {
		rh.Harness = hv.Harness
		fromPackage("harness")
	}
	if rh.Args == nil && hv.Args != nil {
		rh.Args = hv.Args
		fromPackage("args")
	}
	if rh.Argv == nil && hv.Argv != nil {
		rh.Argv = hv.Argv
		fromPackage("argv")
	}
	if rh.Model == "" && hv.Model != "" {
		rh.Model = hv.Model
		fromPackage("model")
	}
	if rh.AutoAccept == nil && hv.AutoAccept != nil {
		rh.AutoAccept = hv.AutoAccept
		fromPackage("auto_accept")
	}
	if rh.MaxTurns == nil && hv.MaxTurns != nil {
		rh.MaxTurns = hv.MaxTurns
		fromPackage("max_turns")
	}
	if rh.Quiet == nil && hv.Quiet != nil {
		rh.Quiet = hv.Quiet
		fromPackage("quiet")
	}
	if rh.SystemPromptFile == "" && hv.SystemPromptFile != "" {
		rh.SystemPromptFile = manifestPath(hv.SystemPromptFile)
		fromPackage("system_prompt_file")
	}
	if rh.MCPConfig == "" && hv.MCPConfig != "" {
		rh.MCPConfig = manifestPath(hv.MCPConfig)
		fromPackage("mcp_config")
	}
	if rh.AllowedTools == nil && hv.AllowedTools != nil {
		rh.AllowedTools = hv.AllowedTools
		fromPackage("allowed_tools")
	}
	sort.Strings(rh.PackageKeys)
	return rh, nil
}
