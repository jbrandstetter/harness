package config

// Scrollback Byte Budget
//
// [daemon] scrollback_bytes sizes each harness's attach scrollback ring
// (ADR-0007). Its value is owned by internal/settings, which resolves it flag >
// env > file > default (ADR-0016); this file is where its range lives, so the
// file is checked here, with its source line, on the initial load and on every
// reload, and cmd/harness applies the same check to a flag or HARNESS_*
// value.
//
// The floor keeps a ring big enough to hold one capped line with room to
// evict around it. The ceiling catches a unit typo ("16GiB" for "16MiB"),
// which would otherwise reserve that much per harness.
//
// Governing: ADR-0007 (byte-bounded scrollback ring), ADR-0016, SPEC-0010 REQ
// "Environment Value Validation".
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"github.com/stump-wtf/harness/internal/settings"
)

// Accepted range for a scrollback byte budget.
const (
	MinScrollbackBytes = 64 << 10
	MaxScrollbackBytes = 1 << 30
)

// scrollbackRangeErr is the reason a budget is refused, shared by every source.
type scrollbackRangeErr int64

func (e scrollbackRangeErr) Error() string {
	return "must be between " + settings.FormatBytes(MinScrollbackBytes) + " and " +
		settings.FormatBytes(MaxScrollbackBytes) + ", got " + settings.FormatBytes(int64(e))
}

// CheckScrollbackBytes validates a scrollback byte budget, whatever its source.
func CheckScrollbackBytes(n int64) error {
	if n < MinScrollbackBytes || n > MaxScrollbackBytes {
		return scrollbackRangeErr(n)
	}
	return nil
}

// checkScrollbackBytes validates [daemon] scrollback_bytes as decoded: a TOML
// integer counts bytes, a string is a size such as "4MiB". nil means absent.
func checkScrollbackBytes(filename string, data []byte, v any) error {
	if v == nil {
		return nil
	}
	fail := func(format string, args ...any) error {
		return newError(filename, lineOfKeyInTable(data, "daemon", "scrollback_bytes"),
			"[daemon] scrollback_bytes: "+format, args...)
	}
	var n int64
	switch x := v.(type) {
	case int64:
		n = x
	case string:
		parsed, err := settings.ParseBytes(x)
		if err != nil {
			return fail("%v", err)
		}
		n = parsed
	default:
		return fail("expected a size such as \"4MiB\" or a whole number of bytes, got %v", v)
	}
	if err := CheckScrollbackBytes(n); err != nil {
		return fail("%v", err)
	}
	return nil
}
