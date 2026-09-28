package main

// Scrollback Byte Budget, End to End
//
// --scrollback-bytes, HARNESS_SCROLLBACK_BYTES and [daemon] scrollback_bytes
// resolve through the ADR-0016 ladder into daemonOpts, are range-checked from
// every source, survive the --detach re-exec, and are what the daemon's attach
// registry is built with.
//
// Governing: ADR-0007 (byte-bounded scrollback ring), ADR-0016, SPEC-0010 REQ
// "Precedence Order", REQ "Environment Value Validation".
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/attach"
)

// resolveDaemonFor runs resolveDaemonSettings, the function runDaemonCmd calls,
// on the daemon's own command with args typed.
func resolveDaemonFor(t *testing.T, args ...string) (daemonOpts, error) {
	t.Helper()
	g := &globalOpts{}
	cmd := newDaemonCmd(g)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	d := daemonOpts{}
	err := resolveDaemonSettings(cmd, g, &d)
	return d, err
}

func TestScrollbackBytesPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	t.Setenv("HARNESS_CONFIG", cfgPath)
	t.Setenv("HARNESS_SCROLLBACK_BYTES", "")

	d, err := resolveDaemonFor(t)
	if err != nil {
		t.Fatal(err)
	}
	if d.ringBytes != attach.DefaultRingBytes {
		t.Errorf("no file, no env, no flag: ringBytes = %d, want the default %d", d.ringBytes, attach.DefaultRingBytes)
	}

	if err := os.WriteFile(cfgPath, []byte("[daemon]\nscrollback_bytes = \"2MiB\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := resolveDaemonFor(t); err != nil || d.ringBytes != 2<<20 {
		t.Errorf("file: ringBytes = %d (%v), want 2 MiB", d.ringBytes, err)
	}

	t.Setenv("HARNESS_SCROLLBACK_BYTES", "3MiB")
	if d, err := resolveDaemonFor(t); err != nil || d.ringBytes != 3<<20 {
		t.Errorf("env over file: ringBytes = %d (%v), want 3 MiB", d.ringBytes, err)
	}

	d, err = resolveDaemonFor(t, "--scrollback-bytes", "4MiB")
	if err != nil || d.ringBytes != 4<<20 {
		t.Fatalf("flag over env: ringBytes = %d (%v), want 4 MiB", d.ringBytes, err)
	}
	if args := strings.Join(d.childArgs(), " "); !strings.Contains(args, "--scrollback-bytes 4MiB") {
		t.Errorf("--detach child args drop the budget: %s", args)
	}
	if lim := daemonAttachRegistry(d).Limits(); lim.Bytes != 4<<20 || lim.Lines != d.ringLines {
		t.Errorf("the daemon's registry got %+v, want %d bytes and %d lines", lim, 4<<20, d.ringLines)
	}
}

func TestScrollbackBytesRejectedFromEverySource(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	t.Setenv("HARNESS_CONFIG", cfgPath)

	for _, tc := range []struct {
		name string
		file string
		env  string
		args []string
		want []string
	}{
		{name: "flag below the floor", args: []string{"--scrollback-bytes", "16KiB"},
			want: []string{"--scrollback-bytes", "between 64KiB and 1GiB", "16KiB"}},
		{name: "flag not a size", args: []string{"--scrollback-bytes", "lots"},
			want: []string{"--scrollback-bytes", `invalid size "lots"`}},
		{name: "env over the ceiling", env: "2GiB",
			want: []string{"HARNESS_SCROLLBACK_BYTES", "between 64KiB and 1GiB", "2GiB"}},
		{name: "file below the floor", file: "[daemon]\nscrollback_bytes = 4096\n",
			want: []string{"daemon.scrollback_bytes", "between 64KiB and 1GiB", "4KiB"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(cfgPath, []byte(tc.file), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HARNESS_SCROLLBACK_BYTES", tc.env)
			_, err := resolveDaemonFor(t, tc.args...)
			if err == nil {
				t.Fatal("resolved; want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}
