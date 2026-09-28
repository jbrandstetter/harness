package main

// Sealed Log Compression Wiring
//
// Compression is on by default (ADR-0007 as amended), and the Manager option
// that turns it on defaults to OFF — so the only thing standing between a
// production daemon and uncompressed logs is this wiring. These tests resolve
// the daemon's settings the way a bare `harness daemon` does and assert on the
// options runDaemon builds its Manager from; asserting on the settings
// registry's default alone would pass with the wiring missing, which is the
// shape of #315 (TestDaemonManagerOptionsEnableGiveUp).
//
// Governing: ADR-0007 (as amended for sealed compression), ADR-0016;
// SPEC-0010 REQ "Environment Variable Namespace", REQ "Precedence Order";
// SPEC-0003 REQ "Durable Log Rotation And Compression"; GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/config"
)

// resolveDaemonOpts resolves a daemon's settings from args, the environment
// and the file HARNESS_CONFIG names, as runDaemonCmd does.
func resolveDaemonOpts(t *testing.T, args ...string) daemonOpts {
	t.Helper()
	g := &globalOpts{}
	cmd := newDaemonCmd(g)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	d := daemonOpts{}
	if err := resolveDaemonSettings(cmd, g, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDaemonCompressesLogsByDefault(t *testing.T) {
	t.Setenv("HARNESS_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("HARNESS_COMPRESS_LOGS", "")
	d := resolveDaemonOpts(t)
	if opts := daemonManagerOptionsFor(attach.NewRegistry(100), d); !opts.CompressLogs {
		t.Fatal("a daemon with no flag, variable or file setting does not compress sealed logs; the default is on")
	}
	if args := strings.Join(d.childArgs(), " "); !strings.Contains(args, "--compress-logs=true") {
		t.Errorf("--detach child args do not carry the resolved setting: %s", args)
	}
}

// TestDaemonCompressLogsOptOut: each rung of the ladder can turn it off, and
// the file key is one harness.toml's strict decode accepts.
func TestDaemonCompressLogsOptOut(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfgPath, []byte("[daemon]\ncompress_logs = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("harness.toml refused [daemon] compress_logs: %v", err)
	}
	reg := attach.NewRegistry(100)

	t.Setenv("HARNESS_CONFIG", cfgPath)
	t.Setenv("HARNESS_COMPRESS_LOGS", "")
	if d := resolveDaemonOpts(t); daemonManagerOptionsFor(reg, d).CompressLogs {
		t.Error("[daemon] compress_logs = false did not turn compression off")
	}

	t.Setenv("HARNESS_CONFIG", filepath.Join(dir, "absent.toml"))
	t.Setenv("HARNESS_COMPRESS_LOGS", "0")
	d := resolveDaemonOpts(t)
	if daemonManagerOptionsFor(reg, d).CompressLogs {
		t.Error("HARNESS_COMPRESS_LOGS=0 did not turn compression off")
	}
	if args := strings.Join(d.childArgs(), " "); !strings.Contains(args, "--compress-logs=false") {
		t.Errorf("--detach child args drop the opt-out: %s", args)
	}

	t.Setenv("HARNESS_COMPRESS_LOGS", "")
	if d := resolveDaemonOpts(t, "--compress-logs=false"); daemonManagerOptionsFor(reg, d).CompressLogs {
		t.Error("--compress-logs=false did not turn compression off")
	}
	t.Setenv("HARNESS_CONFIG", cfgPath)
	if d := resolveDaemonOpts(t, "--compress-logs"); !daemonManagerOptionsFor(reg, d).CompressLogs {
		t.Error("--compress-logs did not beat the file's compress_logs = false")
	}
}

func TestConfigRejectsNonBooleanCompressLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte("[daemon]\ncompress_logs = \"sometimes\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Error("a non-boolean compress_logs loaded")
	}
}
