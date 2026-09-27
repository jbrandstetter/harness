package main

// [daemon] Process Settings, End to End
//
// The file layer for scrollback, log_level, log_file and socket existed in the
// settings registry but no daemon could start with any of them in harness.toml:
// internal/config refused each as an unknown key (GitHub stump-wtf/harness#19).
// These tests run the path the daemon actually takes, so they fail if either
// reader stops accepting a key or the resolved value stops reaching its
// consumer.
//
// Governing: ADR-0016, SPEC-0010 REQ "Precedence Order".
//
// @joestump-agent 09/27/2026 - Added for GitHub stump-wtf/harness#19.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDaemonTableSettingsReachDaemonOpts drives resolveDaemonSettings, the
// function runDaemonCmd calls, with the daemon's own command. Values from
// [daemon] must land in daemonOpts, and an environment variable must still
// outrank the file.
func TestDaemonTableSettingsReachDaemonOpts(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	sock := filepath.Join(dir, "from-file.sock")
	logFile := filepath.Join(dir, "daemon.log")
	body := "[daemon]\nscrollback = 2000\nlog_level = \"debug\"\n" +
		"log_file = \"" + logFile + "\"\nsocket = \"" + sock + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", cfgPath)
	for _, k := range []string{"HARNESS_SOCKET", "HARNESS_SCROLLBACK", "HARNESS_LOG_LEVEL", "HARNESS_LOG_FILE"} {
		t.Setenv(k, "")
	}

	resolve := func() daemonOpts {
		t.Helper()
		g := &globalOpts{}
		cmd := newDaemonCmd(g)
		if err := cmd.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		d := daemonOpts{}
		if err := resolveDaemonSettings(cmd, g, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	d := resolve()
	if d.ringLines != 2000 || d.logLevel != "debug" || d.logFile != logFile || d.socketPath != sock {
		t.Errorf("from file: ring=%d level=%q file=%q socket=%q; want 2000, debug, %q, %q",
			d.ringLines, d.logLevel, d.logFile, d.socketPath, logFile, sock)
	}

	t.Setenv("HARNESS_SCROLLBACK", "500")
	if d := resolve(); d.ringLines != 500 {
		t.Errorf("HARNESS_SCROLLBACK=500 over [daemon] scrollback = 2000: ring = %d, want 500", d.ringLines)
	}
}

// TestDaemonStartsOnDaemonTableSettings is the report itself: a daemon given
// only --config, whose file sets [daemon] scrollback and socket, must start and
// bind the file's socket, and a client given the same --config must find it.
func TestDaemonStartsOnDaemonTableSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	dir := shortSockDir(t)
	sock := filepath.Join(dir, "file.sock")
	cfg := filepath.Join(dir, "harness.toml")
	body := "[daemon]\nscrollback = 2000\nlog_level = \"warn\"\nsocket = \"" + sock + "\"\n\n" +
		"[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	d := startDaemonProc(t, bin, env, "--config", cfg, "daemon", "run")
	if err := d.awaitReady(bin, env, sock, daemonStartupCeiling(t)); err != nil {
		t.Fatalf("daemon with [daemon] scrollback/log_level/socket in %s: %v", cfg, err)
	}

	out, code := runCLI(t, bin, env, "--config", cfg, "list")
	if code != 0 || !strings.Contains(out, "demo") {
		t.Errorf("`harness --config %s list` (exit %d) did not reach the daemon on the file's socket:\n%s", cfg, code, out)
	}
}
