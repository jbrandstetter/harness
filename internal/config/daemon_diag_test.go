package config

// Daemon Memory Guardrail Key Tests
//
// [daemon] memory_limit and pprof_addr load when valid and are refused, with
// the key's own line, when not. The line matters: it is what the reload banner
// shows (SPEC-0001 REQ "Zero And Error States").
//
// Governing: SPEC-0010 REQ "Go Memory Limit"; SPEC-0013 REQ-8.
//
// @joestump-agent 09/28/2026 - Added with daemon_diag.go.

import (
	"errors"
	"strings"
	"testing"
)

func TestDaemonDiagKeysAccepted(t *testing.T) {
	for _, body := range []string{
		`memory_limit = "2GiB"`,
		`memory_limit = "512MiB"`,
		`memory_limit = 0`,
		`memory_limit = 2147483648`,
		`memory_limit = "0"`,
		`pprof_addr = "127.0.0.1:6060"`,
		`pprof_addr = "localhost:6060"`,
		`pprof_addr = "[::1]:6060"`,
		`pprof_addr = ""`,
	} {
		src := "[daemon]\nwatch_config = true\n" + body + "\n"
		if _, err := Parse([]byte(src), "good.toml"); err != nil {
			t.Errorf("%s: rejected: %v", body, err)
		}
	}
}

func TestDaemonDiagKeysRefusedWithLine(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []string
	}{
		{`memory_limit = "lots"`, []string{"memory_limit", `"lots"`, "GiB"}},
		{`memory_limit = "1.5GiB"`, []string{"memory_limit", "1.5GiB"}},
		{`memory_limit = -1`, []string{"memory_limit", "-1"}},
		{`memory_limit = 1.5`, []string{"memory_limit", "float64"}},
		{`memory_limit = true`, []string{"memory_limit", "bool"}},
		{`pprof_addr = "0.0.0.0:6060"`, []string{"pprof_addr", "loopback"}},
		{`pprof_addr = ":6060"`, []string{"pprof_addr", "loopback"}},
		{`pprof_addr = "10.1.2.3:6060"`, []string{"pprof_addr", "loopback"}},
		{`pprof_addr = "127.0.0.1"`, []string{"pprof_addr", "host:port"}},
	} {
		// Line 3: the key under test, below the header and another key.
		src := "[daemon]\nwatch_config = true\n" + tc.body + "\n"
		_, err := Parse([]byte(src), "bad.toml")
		if err == nil {
			t.Errorf("%s: loaded, want a refusal", tc.body)
			continue
		}
		var ce *Error
		if !errors.As(err, &ce) || ce.Line != 3 {
			t.Errorf("%s: error %v, want a *config.Error on line 3", tc.body, err)
		}
		for _, w := range tc.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q does not mention %s", tc.body, err, w)
			}
		}
	}
}
