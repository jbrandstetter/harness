package main

// Settings Resolution Wiring
//
// Where the Cobra flag sets meet the ADR-0016 precedence ladder. Every process
// setting lands here, and nothing downstream needs to know whether a value came
// from a flag, the environment, the file, or a default.
//
// The order this runs in matters. Flags are bound first (so an explicitly typed
// flag can win), then the config file is located — itself a resolved setting,
// since HARNESS_CONFIG may name it — and only then is the file read for its
// scalar keys. A missing file is not an error: SPEC-0010 REQ "Fileless
// Operation" requires a container with no TOML at all to come up on environment
// variables alone.
//
// Governing: ADR-0016, SPEC-0010 REQ "Precedence Order", REQ "Fileless
// Operation".
//
// @joestump-agent 08/19/2026 - Introduced with the ADR-0016 environment layer.
//
// @joestump-agent 09/28/2026 - memory-limit and pprof-addr (GitHub
// https://github.com/stump-wtf/harness/issues/18); a non-loopback pprof
// address is refused here, named by its source.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/diag"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/settings"
)

// newResolver builds a resolver seeded with the runtime-computed defaults. The
// socket and config defaults depend on the XDG environment and the scrollback
// limits come from the attach package, so none of them can be a compile-time
// constant in the registry.
func newResolver(cmd *cobra.Command) *settings.Resolver {
	r := settings.New()
	r.SetDefault("socket", protocol.DefaultSocketPath())
	r.SetDefault("config", config.DefaultPath())
	r.SetDefault("scrollback", attach.DefaultRingLines)
	r.SetDefault("scrollback-bytes", int64(attach.DefaultRingBytes))
	if cmd != nil {
		r.BindFlags(cmd.Flags())
	}
	return r
}

// resolveGlobals fills the persistent flags from the ladder. Called by every
// command's PersistentPreRunE so a bare `harness ls` under HARNESS_SOCKET
// behaves the same as `harness --socket … ls`.
func resolveGlobals(cmd *cobra.Command, g *globalOpts) error {
	r := newResolver(cmd)

	configPath, err := r.String("config")
	if err != nil {
		return err
	}
	// Read the file before resolving the rest, so file-backed settings can
	// contribute. Absence is expected and fine; a real parse failure is not.
	if err := r.ReadConfigFile(configPath); err != nil && !errors.Is(err, settings.ErrNoConfigFile) {
		return err
	}

	socket, err := r.String("socket")
	if err != nil {
		return err
	}
	jsonOut, err := r.Bool("json")
	if err != nil {
		return err
	}

	g.configPath, g.socket, g.json = configPath, socket, jsonOut
	// Re-apply now that HARNESS_JSON and the file have had their say; main()
	// seeded this from a raw argv scan so help could render correctly before
	// this ran.
	cliui.SetJSON(jsonOut)
	return nil
}

// resolveDaemonSettings fills the daemon's settings from the ladder, on top of
// the globals.
func resolveDaemonSettings(cmd *cobra.Command, g *globalOpts, d *daemonOpts) error {
	r := newResolver(cmd)

	configPath, err := r.String("config")
	if err != nil {
		return err
	}
	if err := r.ReadConfigFile(configPath); err != nil && !errors.Is(err, settings.ErrNoConfigFile) {
		return err
	}

	socket, err := r.String("socket")
	if err != nil {
		return err
	}
	ring, err := r.Int("scrollback")
	if err != nil {
		return err
	}
	ringBytes, err := resolveScrollbackBytes(r)
	if err != nil {
		return err
	}
	sshEnable, err := r.Bool("ssh")
	if err != nil {
		return err
	}
	sshListen, err := r.String("ssh-listen")
	if err != nil {
		return err
	}
	// Only a flag or HARNESS_WEBHOOK_LISTEN is carried as an override. The
	// file's value is read by internal/config with the rest of [server], and
	// re-read on every reload; carrying it here too would freeze it at boot
	// and hide a reload's change to it (see daemonOpts.webhookListen).
	webhook, err := r.Resolve("webhook-listen")
	if err != nil {
		return err
	}
	webhookListen := ""
	if webhook.Source == settings.SourceFlag || webhook.Source == settings.SourceEnv {
		webhookListen, _ = webhook.Value.(string)
	}
	logLevel, err := r.String("log-level")
	if err != nil {
		return err
	}
	logFile, err := r.String("log-file")
	if err != nil {
		return err
	}
	compressLogs, err := r.Bool("compress-logs")
	if err != nil {
		return err
	}
	// The source is kept with the value: an unset limit leaves GOMEMLIMIT in
	// charge, where an explicit one (even "0") overrides it (daemon_memory.go).
	memLimit, err := r.Resolve("memory-limit")
	if err != nil {
		return err
	}
	pprof, err := r.Resolve("pprof-addr")
	if err != nil {
		return err
	}
	pprofAddr, _ := pprof.Value.(string)
	if err := diag.CheckPprofAddr(pprofAddr); err != nil {
		// Named by source, like every other bad value (SPEC-0010 REQ "Error
		// Handling Standards"), and refused before anything starts.
		return fmt.Errorf("%s: %w", settingOrigin(pprof), err)
	}

	d.configPath, d.socketPath = configPath, socket
	d.ringLines, d.ringBytes, d.sshEnable, d.sshListen = ring, ringBytes, sshEnable, sshListen
	d.webhookListen = webhookListen
	d.logLevel, d.logFile = logLevel, logFile
	d.compressLogs = compressLogs
	d.memoryLimit, _ = memLimit.Value.(int64)
	d.memoryLimitSource = memLimit.Source
	d.pprofAddr = strings.TrimSpace(pprofAddr)

	g.configPath, g.socket = configPath, socket
	return nil
}

// settingOrigin names where a resolved value came from, spelled the way the
// operator wrote it: --flag, HARNESS_VAR, or the file key.
func settingOrigin(r settings.Resolved) string {
	switch r.Source {
	case settings.SourceFlag:
		return "--" + r.Setting.Name
	case settings.SourceEnv:
		return r.Setting.Env
	case settings.SourceFile:
		return r.Setting.FileKey
	}
	return r.Setting.Name
}

// resolveScrollbackBytes resolves the scrollback ring's byte budget and holds it
// to the range internal/config holds the file to, naming the source that
// supplied it (SPEC-0010 REQ "Environment Value Validation"). This runs before
// the daemon loads its config, so a bad file value stops startup here, named
// by its key; config.Load would name its line.
func resolveScrollbackBytes(r *settings.Resolver) (int64, error) {
	got, err := r.Resolve("scrollback-bytes")
	if err != nil {
		return 0, err
	}
	n, _ := got.Value.(int64)
	if err := config.CheckScrollbackBytes(n); err != nil {
		origin := got.Setting.FileKey
		switch got.Source {
		case settings.SourceFlag:
			origin = "--" + got.Setting.Name
		case settings.SourceEnv:
			origin = got.Setting.Env
		}
		return 0, fmt.Errorf("%s: %w", origin, err)
	}
	return n, nil
}

// resolveReport returns every setting with its winning source, for `harness
// doctor` (SPEC-0010 REQ "Source Attribution").
func resolveReport(cmd *cobra.Command) ([]settings.Resolved, error) {
	r := newResolver(cmd)

	configPath, err := r.String("config")
	if err != nil {
		return nil, err
	}
	if err := r.ReadConfigFile(configPath); err != nil && !errors.Is(err, settings.ErrNoConfigFile) {
		return nil, err
	}
	return r.ResolveAll()
}
