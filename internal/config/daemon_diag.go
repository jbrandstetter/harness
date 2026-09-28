package config

// Daemon Memory Guardrail Keys
//
// Validates [daemon] memory_limit and pprof_addr. Like the other process
// settings, their value is owned by internal/settings, which ranks a flag or
// HARNESS_* variable above this file (ADR-0016), and they take effect at the
// next daemon start. This pass exists so a bad value is refused here, with its
// line number, by the initial load and by every reload. Without it, a reload
// would accept pprof_addr = "0.0.0.0:6060" and the daemon would refuse to
// start on its next restart, long after anyone connected the two.
//
// memory_limit takes a string with a unit ("2GiB") or a bare TOML integer of
// bytes (0 is off), so the raw field is untyped and read through the same
// settings.ParseBytes the resolver uses.
//
// Governing: SPEC-0010 REQ "Go Memory Limit", REQ "Environment Value
// Validation"; SPEC-0013 REQ-8 (pprof binds loopback only).
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"fmt"

	"github.com/stump-wtf/harness/internal/diag"
	"github.com/stump-wtf/harness/internal/settings"
)

// checkDaemonDiag validates rd's memory_limit and pprof_addr.
func checkDaemonDiag(filename string, data []byte, rd rawDaemon) error {
	if rd.MemoryLimit != nil {
		v := rd.MemoryLimit
		switch v.(type) {
		case string, int64:
		default:
			// A float or a bool would pass fmt.Sprint into ParseBytes as
			// "1.5" or "true" and fail there anyway; saying what TOML type
			// was given is the clearer message.
			return newError(filename, lineOfKeyInTable(data, "daemon", "memory_limit"),
				"[daemon] memory_limit: want a size string such as \"2GiB\" or an integer number of bytes, got %T %v", v, v)
		}
		if _, err := settings.ParseBytes(fmt.Sprint(v)); err != nil {
			return newError(filename, lineOfKeyInTable(data, "daemon", "memory_limit"), "[daemon] memory_limit: %v", err)
		}
	}
	if rd.PprofAddr != nil {
		if err := diag.CheckPprofAddr(*rd.PprofAddr); err != nil {
			return newError(filename, lineOfKeyInTable(data, "daemon", "pprof_addr"), "[daemon] pprof_addr: %v", err)
		}
	}
	return nil
}
