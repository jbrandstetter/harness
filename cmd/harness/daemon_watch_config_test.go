package main

// Config Watch Wiring
//
// HARNESS_WATCH_CONFIG sat in the settings registry, so `harness doctor`
// reported it, while the daemon read only [daemon] watch_config: setting the
// variable changed the report and nothing else. These tests resolve the
// daemon's settings the way `harness daemon` does and assert on the decision
// runDaemon makes, not on the registry, which was right all along (#315).
//
// Governing: ADR-0016; SPEC-0010 REQ "Precedence Order" (scenario
// "Environment beats file").

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
)

func TestDaemonWatchConfigLadder(t *testing.T) {
	dir := t.TempDir()
	off := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(off, []byte("[daemon]\nwatch_config = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent.toml")

	// watches resolves the daemon's settings under cfgPath and env, loads the
	// file as runDaemon does, and returns runDaemon's decision.
	watches := func(t *testing.T, cfgPath, env string) bool {
		t.Helper()
		t.Setenv("HARNESS_CONFIG", cfgPath)
		t.Setenv("HARNESS_WATCH_CONFIG", env)
		var dc core.DaemonConfig
		if cfgPath != absent {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			dc = cfg.Daemon
		}
		return daemonWatchesConfig(dc, resolveDaemonOpts(t))
	}

	cases := []struct {
		name, cfg, env string
		want           bool
	}{
		{"default", absent, "", true},
		{"file off", off, "", false},
		{"environment beats file", off, "1", true},
		{"environment off", absent, "false", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := watches(t, tc.cfg, tc.env); got != tc.want {
				t.Errorf("config %s, HARNESS_WATCH_CONFIG=%q: watches = %v, want %v", filepath.Base(tc.cfg), tc.env, got, tc.want)
			}
		})
	}
}

// TestDaemonRefusesBadWatchConfigEnv: a malformed value is refused, named by
// the variable, rather than silently read as the default.
func TestDaemonRefusesBadWatchConfigEnv(t *testing.T) {
	t.Setenv("HARNESS_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("HARNESS_WATCH_CONFIG", "sometimes")
	g := &globalOpts{}
	cmd := newDaemonCmd(g)
	d := daemonOpts{}
	err := resolveDaemonSettings(cmd, g, &d)
	if err == nil || !strings.Contains(err.Error(), "HARNESS_WATCH_CONFIG") {
		t.Fatalf("HARNESS_WATCH_CONFIG=sometimes: err = %v, want a refusal naming the variable", err)
	}
}
