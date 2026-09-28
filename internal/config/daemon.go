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
// Governing: ADR-0016 (Cobra for commands, Viper for process config), SPEC-0010
// REQ "Precedence Order", REQ "Environment Value Validation".
//
// @joestump-agent 09/27/2026 - Added for GitHub stump-wtf/harness#19.

import (
	"path/filepath"

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
	return checkScrollbackBytes(filename, data, rd.ScrollbackBytes)
}
