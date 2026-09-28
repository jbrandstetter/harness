package main

// Doctor's Settings Report Under --config
//
// Governing: ADR-0016, SPEC-0010 REQ "Source Attribution".
//
// @joestump-agent 09/27/2026 - Added with the fix that passes doctor's command
// to its settings report.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoctorHonoursConfigFlag pins doctor's settings report to the --config it
// was given. It used to resolve the report with no command, so it read the
// default config path and reported every file-backed setting as "default".
func TestDoctorHonoursConfigFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	cfg := filepath.Join(t.TempDir(), "custom.toml")
	if err := os.WriteFile(cfg, []byte("[daemon]\nscrollback = 2000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortSockDir(t), "none.sock")
	out, _ := runCLI(t, bin, append(env, "HARNESS_SCROLLBACK="), "--config", cfg, "--socket", socket, "doctor", "--json")

	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in doctor output:\n%s", out)
	}
	var got struct {
		Settings map[string]struct {
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &got); err != nil {
		t.Fatalf("parse doctor JSON: %v\n%s", err, out[start:])
	}
	for name, want := range map[string]struct{ value, source string }{
		"config":     {cfg, "flag"},
		"socket":     {socket, "flag"},
		"scrollback": {"2000", "file"},
	} {
		s := got.Settings[name]
		if s.Value != want.value || s.Source != want.source {
			t.Errorf("%s = %q from %q, want %q from %q", name, s.Value, s.Source, want.value, want.source)
		}
	}
}
