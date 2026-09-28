package config

// Daemon Process Settings Tests
//
// harness.toml has two readers: this package's strict decode, and the Viper
// file layer in internal/settings. A key only works if BOTH accept it, and for
// the [daemon] process settings they disagreed from the day the settings layer
// landed. internal/settings' own ladder test hands Viper the file directly, so
// it passed the whole time a real daemon refused to start on the same file
// (GitHub stump-wtf/harness#19).
//
// TestRegistryFileKeysLoad closes that gap for every file-backed setting at
// once. It writes each FileKey into one file and requires both readers to take
// it, so a setting added to the registry without a field here fails at once.
//
// Governing: ADR-0016, SPEC-0010 REQ "Precedence Order", REQ "Environment
// Value Validation".
//
// @joestump-agent 09/27/2026 - Added for GitHub stump-wtf/harness#19.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/settings"
)

// fileKeySamples is a valid, non-default TOML value for each file-backed
// setting, keyed by setting name. A new FileKey with no sample fails the test
// rather than being skipped.
var fileKeySamples = map[string]any{
	"socket":           "/tmp/harness-test.sock",
	"log-level":        "warn",
	"log-file":         "/tmp/harness-test.log",
	"scrollback":       2000,
	"scrollback-bytes": int64(2 << 20), // KindBytes resolves to int64
	"ssh":              false,
	"ssh-listen":       "127.0.0.1:2222",
	"webhook-listen":   "127.0.0.1:9000",
	"watch-config":     false,
	"compress-logs":    false,
}

func TestRegistryFileKeysLoad(t *testing.T) {
	tables := map[string][]string{}
	want := map[string]any{}
	for _, s := range settings.Registry {
		if s.FileKey == "" {
			continue
		}
		v, ok := fileKeySamples[s.Name]
		if !ok {
			t.Fatalf("setting %q has FileKey %q but no entry in fileKeySamples", s.Name, s.FileKey)
		}
		table, key, _ := strings.Cut(s.FileKey, ".")
		lit := fmt.Sprint(v)
		if str, isStr := v.(string); isStr {
			lit = fmt.Sprintf("%q", str)
		}
		tables[table] = append(tables[table], key+" = "+lit)
		want[s.Name] = v
		// An exported variable would outrank the file and hide a miss.
		t.Setenv(s.Env, "")
	}

	names := make([]string, 0, len(tables))
	for name := range tables {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		fmt.Fprintf(&b, "[%s]\n%s\n\n", name, strings.Join(tables[name], "\n"))
	}
	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err != nil {
		t.Fatalf("Load refused a file carrying every settings FileKey:\n%s\nerror: %v", b.String(), err)
	}

	r := settings.New()
	if err := r.ReadConfigFile(path); err != nil {
		t.Fatal(err)
	}
	for name, v := range want {
		got, err := r.Resolve(name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if got.Source != settings.SourceFile || got.Value != v {
			t.Errorf("%s = %v from %s, want %v from file", name, got.Value, got.Source, v)
		}
	}
}

func TestDaemonSettingsRejected(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"unknown log level", `log_level = "verbose"`, `daemon.log_level: invalid value "verbose"`},
		{"zero scrollback", `scrollback = 0`, "scrollback: must be at least 1"},
		{"scrollback not an integer", `scrollback = "lots"`, "[daemon]"},
		{"relative socket", `socket = "harness.sock"`, "socket: must be an absolute path"},
		{"tilde log file", `log_file = "~/harness.log"`, "log_file: must be an absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte("[daemon]\nwatch_config = true\n"+tt.line+"\n"), "harness.toml")
			if err == nil {
				t.Fatalf("%s loaded; want an error", tt.line)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
			var ce *Error
			if tt.name != "scrollback not an integer" && (!errors.As(err, &ce) || ce.Line != 3) {
				t.Errorf("error %q does not point at line 3", err)
			}
		})
	}
}
