package main

// Daemon Memory Guardrails Wiring
//
// Applies the daemon's Go soft memory limit and starts the opt-in pprof
// listener, both from settings resolved before runDaemon (settings_wire.go).
// The policy lives in internal/diag; this file owns when each happens in the
// daemon's life and what the log says about it.
//
//   - The memory limit is applied first thing after the logger, before
//     anything allocates in earnest, and the limit actually in effect is read
//     back from the runtime and logged with its source. An operator asking
//     "is the limit on?" gets the answer from one startup line, and from
//     go_gc_gomemlimit_bytes on /metrics.
//   - Both are process settings: they are read once at start, and a reload
//     does not re-apply them. Changing either needs a daemon restart, as for
//     the log level and every listener.
//   - pprof binds after the daemon's refusal checks, so a daemon that is about
//     to refuse never serves it. A bind failure is logged and the daemon
//     carries on, as for the metrics listener: a taken port is an
//     observability gap, not a reason to stop supervising. It shuts down last,
//     after the Manager, so the shutdown itself can be profiled.
//
// Functions rather than inline code, like daemonManagerOptions, so the tests
// drive what runDaemon runs (#315).
//
// Governing: SPEC-0010 REQ "Go Memory Limit"; SPEC-0013 REQ-7, REQ-8.
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"context"
	"strings"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/diag"
	"github.com/stump-wtf/harness/internal/settings"
)

// applyDaemonMemoryLimit resolves the limit (a harness setting over
// GOMEMLIMIT over off), applies it, and logs the limit now in effect. getenv
// is os.Getenv in the daemon.
func applyDaemonMemoryLimit(bytes int64, source settings.Source, getenv func(string) string) diag.MemoryLimit {
	explicit := source == settings.SourceFlag || source == settings.SourceEnv || source == settings.SourceFile
	l := diag.ResolveMemoryLimit(bytes, explicit, string(source), getenv)
	effective := diag.ApplyMemoryLimit(l)

	kv := []any{"limit", describeLimit(effective), "source", l.Source}
	if l.Explicit && strings.TrimSpace(getenv("GOMEMLIMIT")) != "" {
		kv = append(kv, "overrides", "GOMEMLIMIT")
	}
	log.Info("memory limit", kv...)
	if l.Explicit && l.Bytes > 0 && l.Bytes < diag.SmallMemoryLimit {
		log.Warn("memory limit is very small; the GC will run almost continuously",
			"limit", describeLimit(l.Bytes),
			"hint", "a bare number is bytes: write memory_limit = \"2GiB\", not 2048")
	}
	return l
}

// describeLimit renders a limit for the log: "off", or IEC units.
func describeLimit(n int64) string {
	if n == 0 {
		return "off"
	}
	return settings.FormatBytes(n)
}

// startDaemonPprof binds the pprof listener when addr is set; nil when it is
// off or could not bind. addr was already checked for loopback by
// resolveDaemonSettings, and ListenPprof checks it again.
func startDaemonPprof(addr string) *diag.PprofServer {
	if addr == "" {
		return nil
	}
	s, err := diag.ListenPprof(addr)
	if err != nil {
		log.Error("pprof listener disabled", "addr", addr, "err", err,
			"hint", "set [daemon] pprof_addr to a free loopback address and restart the daemon")
		return nil
	}
	log.Info("pprof listening", "addr", s.Addr(), "path", "/debug/pprof/", "hint", "go tool pprof http://"+s.Addr()+"/debug/pprof/heap")
	return s
}

// stopDaemonPprof shuts the listener down, cutting off a profile still
// streaming after five seconds. nil is a no-op.
func stopDaemonPprof(s *diag.PprofServer) {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.Shutdown(ctx)
}
