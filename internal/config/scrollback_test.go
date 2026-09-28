package config

// Governing: ADR-0007 (byte-bounded scrollback ring), SPEC-0010 REQ
// "Environment Value Validation" (a bad value fails naming its source).

import (
	"errors"
	"strings"
	"testing"
)

func TestScrollbackBytesAccepted(t *testing.T) {
	for _, line := range []string{
		`scrollback_bytes = "4MiB"`,
		`scrollback_bytes = "64KiB"`,
		`scrollback_bytes = "1GiB"`,
		`scrollback_bytes = "2 m"`,
		`scrollback_bytes = 1048576`,
	} {
		if _, err := Parse([]byte("[daemon]\nwatch_config = true\n"+line+"\n"), "harness.toml"); err != nil {
			t.Errorf("%s: %v", line, err)
		}
	}
}

func TestScrollbackBytesRejected(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{`scrollback_bytes = "32KiB"`, "must be between 64KiB and 1GiB, got 32KiB"},
		{`scrollback_bytes = "16GiB"`, "must be between 64KiB and 1GiB, got 16GiB"},
		{`scrollback_bytes = 0`, "got 0"},
		{`scrollback_bytes = -1`, "got -1B"},
		{`scrollback_bytes = "lots"`, `invalid size "lots"`},
		{`scrollback_bytes = 1.5`, "expected a size"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			_, err := Parse([]byte("[daemon]\nwatch_config = true\n"+tc.line+"\n"), "harness.toml")
			if err == nil {
				t.Fatalf("%s loaded; want an error", tc.line)
			}
			if !strings.Contains(err.Error(), "[daemon] scrollback_bytes") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want [daemon] scrollback_bytes and %q", err, tc.want)
			}
			var ce *Error
			if !errors.As(err, &ce) || ce.Line != 3 {
				t.Errorf("error %q does not point at line 3", err)
			}
		})
	}
}

func TestCheckScrollbackBytes(t *testing.T) {
	for _, n := range []int64{MinScrollbackBytes, 1 << 20, MaxScrollbackBytes} {
		if err := CheckScrollbackBytes(n); err != nil {
			t.Errorf("CheckScrollbackBytes(%d) = %v, want nil", n, err)
		}
	}
	for _, n := range []int64{0, MinScrollbackBytes - 1, MaxScrollbackBytes + 1} {
		if err := CheckScrollbackBytes(n); err == nil {
			t.Errorf("CheckScrollbackBytes(%d) = nil, want an error", n)
		}
	}
}
