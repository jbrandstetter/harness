package config

// Daemon Process Settings
//
// Validates the [daemon] keys whose values internal/settings owns. The value a
// running daemon uses still comes from the settings resolver, which ranks a flag
// or HARNESS_* variable above this file; this pass exists so a bad value is
// refused here, with its source line, by the initial load and by every reload.
//
// Both path keys must be absolute. The CLI and the daemon each read this file
// from their own working directory — a launchd or systemd daemon runs from /,
// the CLI from wherever the operator is — so a relative socket path would name
// a different file for each of them. Nothing expands ~ here either, so a
// "~/..." value would quietly create a directory literally named "~".
//
// memory_limit is read through the same byte-size parser the resolver uses,
// so it takes a size string or a bare TOML integer of bytes. pprof_addr must be
// empty or a loopback host:port: accepted here, a non-loopback value would
// pass a reload and then stop the daemon at its next restart, long after
// anyone connected the two.
//
// Governing: ADR-0016 (Cobra for commands, Viper for process config), SPEC-0010
// REQ "Precedence Order", REQ "Environment Value Validation", REQ "Go Memory
// Limit"; SPEC-0013 REQ-8 (pprof binds loopback only).
//
// @joestump-agent 09/27/2026 - Added for GitHub stump-wtf/harness#19.
//
// @joestump-agent 09/28/2026 - memory_limit and pprof_addr (GitHub
// https://github.com/stump-wtf/harness/issues/18).

import (
	"path/filepath"

	"github.com/stump-wtf/harness/internal/diag"
	"github.com/stump-wtf/harness/internal/settings"
)

// checkDaemonSettings validates rd's process-setting keys.
func checkDaemonSettings(filename string, data []byte, rd rawDaemon) error {
	fail := func(key, format string, args ...any) error {
		return newError(filename, lineOfKeyInTable(data, "daemon", key), "[daemon] "+key+": "+format, args...)
	}
	for _, p := range []struct {
		key string
		v   *string
	}{{"socket", rd.Socket}, {"log_file", rd.LogFile}} {
		if p.v != nil && !filepath.IsAbs(*p.v) {
			return fail(p.key, "must be an absolute path, got %q (the daemon and the CLI resolve a relative path against different working directories, and ~ is not expanded)", *p.v)
		}
	}
	if rd.LogLevel != nil {
		if err := settings.CheckFileValue("daemon.log_level", *rd.LogLevel); err != nil {
			// The settings error already names daemon.log_level and the
			// accepted levels; wrapping it in fail would say the key twice.
			return newError(filename, lineOfKeyInTable(data, "daemon", "log_level"), "%v", err)
		}
	}
	if rd.Scrollback != nil && *rd.Scrollback < 1 {
		return fail("scrollback", "must be at least 1, got %d", *rd.Scrollback)
	}
	if rd.MemoryLimit != nil {
		if err := settings.CheckFileValue("daemon.memory_limit", rd.MemoryLimit); err != nil {
			// Names daemon.memory_limit and the accepted units already.
			return newError(filename, lineOfKeyInTable(data, "daemon", "memory_limit"), "%v", err)
		}
	}
	if rd.PprofAddr != nil {
		if err := diag.CheckPprofAddr(*rd.PprofAddr); err != nil {
			return fail("pprof_addr", "%v", err)
		}
	}
	return checkScrollbackBytes(filename, data, rd.ScrollbackBytes)
}
