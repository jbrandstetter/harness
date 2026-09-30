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

	if rh.Harness == "" {
		rh.Harness = hv.Harness
	}
	if rh.Args == nil {
		rh.Args = hv.Args
	}
	if rh.Argv == nil {
		rh.Argv = hv.Argv
	}
	if rh.Model == "" {
		rh.Model = hv.Model
	}
	if rh.AutoAccept == nil {
		rh.AutoAccept = hv.AutoAccept
	}
	if rh.MaxTurns == nil {
		rh.MaxTurns = hv.MaxTurns
	}
	if rh.Quiet == nil {
		rh.Quiet = hv.Quiet
	}
	if rh.SystemPromptFile == "" {
		rh.SystemPromptFile = manifestPath(hv.SystemPromptFile)
	}
	if rh.MCPConfig == "" {
		rh.MCPConfig = manifestPath(hv.MCPConfig)
	}
	if rh.AllowedTools == nil {
		rh.AllowedTools = hv.AllowedTools
	}
	return rh, nil
}
