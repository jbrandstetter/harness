package settings

// Byte Sizes
//
// Some settings are amounts of memory, and an operator writes those the way
// every other tool on the box does: "2GiB" in the unit file next to
// MemoryMax=2G, "512MiB" for a container. ParseBytes reads that form and
// FormatBytes writes it back, so `harness doctor` shows the value in the
// units the operator used rather than a ten-digit byte count.
//
// Every unit is 1024-based, including K/M/G/T and KB/MB/GB/TB. That matches
// systemd's MemoryMax= and Docker's --memory, which are the limits these
// settings sit beside. Kubernetes reads "2G" as 10^9, so its "Gi" spelling is
// accepted too, meaning the same thing as GiB. The Go runtime's own GOMEMLIMIT
// forms (B, KiB, MiB, GiB, TiB) are a subset, so a value copied from
// GOMEMLIMIT means the same thing here.
//
// Only whole numbers are accepted. "1.5GiB" has an obvious reading, but a
// fraction of a KiB does not, and "1536MiB" is exact.
//
// Governing: SPEC-0010 REQ "Environment Value Validation" (a bad value fails
// naming its source and the accepted form, never coerced).
//
// @joestump-agent 09/28/2026 - Added for [daemon] memory_limit and
// scrollback_bytes (GitHub https://github.com/stump-wtf/harness/issues/18).

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// bytesForm is the accepted form, quoted in every parse error.
const bytesForm = "a whole number of bytes with an optional unit: B, KiB, MiB, GiB, TiB (K/KB/Ki, M/MB/Mi, G/GB/Gi, T/TB/Ti are the same 1024-based units)"

// byteUnits maps a lowercased suffix to its multiplier.
var byteUnits = map[string]int64{
	"": 1, "b": 1,
	"k": 1 << 10, "kb": 1 << 10, "ki": 1 << 10, "kib": 1 << 10,
	"m": 1 << 20, "mb": 1 << 20, "mi": 1 << 20, "mib": 1 << 20,
	"g": 1 << 30, "gb": 1 << 30, "gi": 1 << 30, "gib": 1 << 30,
	"t": 1 << 40, "tb": 1 << 40, "ti": 1 << 40, "tib": 1 << 40,
}

// ParseBytes reads a human byte size: "2GiB", "512MiB", "1g", "4096", "0".
// Case does not matter and a space may sit between the number and the unit.
// A negative, fractional or overflowing number, or an unknown unit, is an
// error naming the accepted form.
func ParseBytes(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	num, unit := s, ""
	if i >= 0 {
		num, unit = s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	}
	if num == "" {
		return 0, fmt.Errorf("invalid size %q: expected %s", raw, bytesForm)
	}
	mult, ok := byteUnits[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size %q: expected %s", raw, bytesForm)
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid size %q: too large", raw)
	}
	return n * mult, nil
}

// FormatBytes writes n in the largest IEC unit that divides it exactly, so the
// result parses back to the same value: 2147483648 is "2GiB", 1610612736 is
// "1536MiB", 1000 is "1000B", and 0 is "0".
func FormatBytes(n int64) string {
	if n == 0 {
		return "0"
	}
	for _, u := range []struct {
		name string
		size int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n%u.size == 0 {
			return strconv.FormatInt(n/u.size, 10) + u.name
		}
	}
	return strconv.FormatInt(n, 10) + "B"
}
