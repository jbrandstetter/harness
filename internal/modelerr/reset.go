package modelerr

// Reset-Time Parser
//
// A quota refusal often says when the allowance comes back. ResetAfter reads
// that instant out of an error mark's note, so the daemon can park a harness
// until then instead of restarting it into the same refusal (SPEC-0021
// REQ-12). It knows the shapes in SPEC-0021 design.md "Reset-time formats":
// Claude Code's "|<epoch>" suffix and "resets 3pm (<zone>)", "try again in
// <duration>" and "retry after <n> seconds", a relayed Retry-After or
// x-ratelimit-reset* header, and "reset(s) at <RFC 3339>". Every shape is tried
// for every adapter, because crush, Claude Code and codex can all relay the
// same provider bodies.
//
// The note is untrusted (ADR-0027 "Security and tenancy"): a prompt-injected
// agent can print any reset it likes. So the answer is bounded whatever the
// input. It is the earliest future instant the note names; it is no reset at
// all when that is under a minute away, since the agent's own retry covers
// it; and it is never more than 8 days out, a later one being clamped and
// flagged so doctor can show the clamp. Numbers are compared before they
// become times, so an epoch or a duration too big for time.Time saturates into
// the clamp instead of wrapping into the past.
//
// The package only parses. Whether a note should be read at all — a quota
// error, never a successful run's output (SPEC-0021 REQ-11) — is the caller's
// decision.
//
// Governing: ADR-0027, SPEC-0021 REQ-11, REQ-12; design.md "Reset-time formats".
//
// @joestump 10/04/2026 - Added for harness#473.

import (
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// minReset is the nearest reset worth parking for (SPEC-0021 REQ-12).
	minReset = time.Minute
	// maxReset is the clamp (SPEC-0021 REQ-12).
	maxReset = 8 * 24 * time.Hour
	// beyond stands for any offset past the clamp. It is never returned.
	beyond = maxReset + time.Second
	// maxZone bounds a zone name before it reaches time.LoadLocation.
	maxZone = 64
)

var (
	// "Claude AI usage limit reached|1790000000": epoch seconds ending the
	// note, or a line of it.
	epochSuffixRe = regexp.MustCompile(`(?m)\|(\d+)[ \t\r]*$`)
	// "resets 3pm (America/Los_Angeles)", "resets 8:20pm": a 12-hour clock
	// time, then an optional IANA zone in parentheses.
	clockRe = regexp.MustCompile(`(?i)\bresets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b(?:\s*\(([a-z][a-z0-9_+-]*(?:/[a-z0-9_+-]+)*)\))?`)
	// "Please try again in 2h13m", "Please retry after 6 seconds": a duration
	// follows, read by leadingDuration.
	relativeRe = regexp.MustCompile(`(?i)\b(?:try again in|retry after)\s+`)
	// "retry-after: 120", `"retry-after":"120"`, "Retry-After: Mon, 28 Sep
	// 2026 20:00:00 GMT": delta seconds or an HTTP date (RFC 9110).
	retryAfterRe = regexp.MustCompile(`(?i)\bretry-after["']?\s*[:=]\s*["']?(\d+(?:\.\d+)?|[a-z]{3},\s*\d{1,2}\s+[a-z]{3}\s+\d{4}\s+\d{2}:\d{2}:\d{2}\s+gmt)`)
	// "quota resets at 2026-09-23T00:00:00Z".
	resetAtRe = regexp.MustCompile(`(?i)\bresets?\s+at\s+(\d{4}-\d{2}-\d{2}[t ]\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:z|[+-]\d{2}:\d{2}))`)
	// "x-ratelimit-reset-tokens: 6m0s", `"X-RateLimit-Reset":"1790640000000"`:
	// an epoch (seconds or milliseconds), delta seconds, or a duration.
	rateLimitResetRe = regexp.MustCompile(`(?i)\bx-ratelimit-reset(?:-requests|-tokens)?["']?\s*[:=]\s*["']?([0-9][0-9a-z.]*)`)
)

// ResetAfter reads the reset time a model error's note names, as of now. ok is
// false when the note names none, when every instant it names has passed, or
// when the earliest one is under a minute away. An instant more than 8 days
// out is returned as now + 8 days with clamped set. When ok, until is in
// [now+1m, now+8d]; otherwise it is the zero time.
//
// now's location is the zone a clock time with no zone of its own is read in
// ("resets 3pm"): the daemon passes its local now. A clock time already past
// today is tomorrow's. adapter selects nothing yet — every shape is one any
// adapter can relay — and is in the signature, as it is in Classify's, so an
// adapter-only shape can land without touching callers.
//
// The REQ-11 scenario "The phrase in a successful run" is the caller's: given
// a note, this parses it, whatever the exit status of the run it came from.
func ResetAfter(adapter, note string, now time.Time) (until time.Time, clamped bool, ok bool) {
	var best time.Duration // the earliest future offset; 0 until one is found
	consider := func(d time.Duration) {
		if d > 0 && (best == 0 || d < best) {
			best = d
		}
	}

	for _, m := range epochSuffixRe.FindAllStringSubmatch(note, -1) {
		consider(epochOffset(m[1], time.Second, now))
	}
	for _, m := range clockRe.FindAllStringSubmatch(note, -1) {
		if d, ok := clockOffset(m[1], m[2], m[3], m[4], now); ok {
			consider(d)
		}
	}
	for _, loc := range relativeRe.FindAllStringIndex(note, -1) {
		if seconds, ok := leadingDuration(note[loc[1]:]); ok {
			consider(secondsOffset(seconds))
		}
	}
	for _, m := range retryAfterRe.FindAllStringSubmatch(note, -1) {
		if seconds, err := strconv.ParseFloat(m[1], 64); err == nil || errors.Is(err, strconv.ErrRange) {
			consider(secondsOffset(seconds))
		} else if at, err := http.ParseTime(httpDate(m[1])); err == nil {
			consider(at.Sub(now))
		}
	}
	for _, m := range resetAtRe.FindAllStringSubmatch(note, -1) {
		if at, err := time.Parse(time.RFC3339Nano, rfc3339(m[1])); err == nil {
			consider(at.Sub(now))
		}
	}
	for _, m := range rateLimitResetRe.FindAllStringSubmatch(note, -1) {
		if d, ok := rateLimitResetOffset(m[1], now); ok {
			consider(d)
		}
	}

	switch {
	case best < minReset: // none found (0), or too near to park for
		return time.Time{}, false, false
	case best > maxReset:
		return now.Add(maxReset), true, true
	}
	return now.Add(best), false, true
}

// epochOffset is how far the epoch in digits (counted in unit) lies from now.
// It compares in the epoch's own unit before making a time.Time, so a number
// past the clamp, or too big for int64, saturates to beyond.
func epochOffset(digits string, unit time.Duration, now time.Time) time.Duration {
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return beyond // digits only, so the error is ErrRange: too big
	}
	limit := now.Add(beyond)
	if unit == time.Millisecond {
		if n > limit.UnixMilli() {
			return beyond
		}
		return time.UnixMilli(n).Sub(now)
	}
	if n > limit.Unix() {
		return beyond
	}
	return time.Unix(n, 0).Sub(now)
}

// clockOffset is how far the next h[:mm]am|pm, in zone (now's location when
// zone is empty), lies from now. ok is false for an impossible time or an
// unknown zone: a reset read in the wrong zone could be off by most of a day,
// so it is not guessed at.
func clockOffset(hour, minute, ampm, zone string, now time.Time) (time.Duration, bool) {
	h, _ := strconv.Atoi(hour)
	mm := 0
	if minute != "" {
		mm, _ = strconv.Atoi(minute)
	}
	if h < 1 || h > 12 || mm > 59 {
		return 0, false
	}
	h %= 12
	if strings.EqualFold(ampm, "pm") {
		h += 12
	}
	loc := now.Location()
	if zone != "" {
		if len(zone) > maxZone {
			return 0, false
		}
		l, err := time.LoadLocation(zone)
		if err != nil {
			return 0, false
		}
		loc = l
	}
	day := now.In(loc)
	at := time.Date(day.Year(), day.Month(), day.Day(), h, mm, 0, 0, loc)
	if !at.After(now) {
		at = time.Date(day.Year(), day.Month(), day.Day()+1, h, mm, 0, 0, loc)
	}
	return at.Sub(now), true
}

// rateLimitResetOffset reads an x-ratelimit-reset* value. A bare number is an
// epoch in milliseconds (OpenRouter) from 1e12, in seconds from 1e9, and delta
// seconds below that; anything else is a duration (OpenAI's "6m0s").
func rateLimitResetOffset(value string, now time.Time) (time.Duration, bool) {
	integer, _, _ := strings.Cut(value, ".")
	if strings.Trim(value, "0123456789.") == "" {
		switch {
		case len(integer) >= 13:
			return epochOffset(integer, time.Millisecond, now), true
		case len(integer) >= 10:
			return epochOffset(integer, time.Second, now), true
		}
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return 0, false
		}
		return secondsOffset(seconds), true
	}
	seconds, ok := leadingDuration(value)
	if !ok {
		return 0, false
	}
	return secondsOffset(seconds), true
}

// secondsOffset turns a non-negative count of seconds into an offset, rounded
// to the nanosecond ("2m59.56s" is not a nanosecond short), saturating to
// beyond before the conversion could overflow.
func secondsOffset(seconds float64) time.Duration {
	if !(seconds <= beyond.Seconds()) { // also catches +Inf
		return beyond
	}
	return time.Duration(math.Round(seconds * float64(time.Second)))
}

// durationUnits are the unit words providers write after a number.
var durationUnits = map[string]time.Duration{
	"ms": time.Millisecond, "millisecond": time.Millisecond, "milliseconds": time.Millisecond,
	"s": time.Second, "sec": time.Second, "secs": time.Second, "second": time.Second, "seconds": time.Second,
	"m": time.Minute, "min": time.Minute, "mins": time.Minute, "minute": time.Minute, "minutes": time.Minute,
	"h": time.Hour, "hr": time.Hour, "hrs": time.Hour, "hour": time.Hour, "hours": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour, "days": 24 * time.Hour,
}

// leadingDuration reads the duration s starts with, in seconds: Go's compact
// "2h13m" and "1.898s", or prose's "3 days 4 hours" and "60 seconds". Parts
// may be separated by spaces, commas and "and". ok is false when s does not
// start with a number and a unit word, so "retry after 5 sessions" and "try
// again in an hour" are not durations.
func leadingDuration(s string) (seconds float64, ok bool) {
	for i := 0; ; {
		j := i
		for j < len(s) && (isDigit(s[j]) || s[j] == '.') {
			j++
		}
		k := j
		for k < len(s) && s[k] == ' ' {
			k++
		}
		u := k
		for u < len(s) && isLetter(s[u]) {
			u++
		}
		unit, known := durationUnits[strings.ToLower(s[k:u])]
		if j == i || !known {
			return seconds, ok
		}
		n, err := strconv.ParseFloat(s[i:j], 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return seconds, ok // "1.2.3"
		}
		seconds += n * unit.Seconds()
		ok = true
		i = u
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if rest := s[i:]; len(rest) >= 4 && strings.EqualFold(rest[:4], "and ") {
			i += 4
		}
	}
}

func isDigit(c byte) bool  { return '0' <= c && c <= '9' }
func isLetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

// httpDate restores the "GMT" http.ParseTime matches literally, after a
// case-insensitive match ("mon, 28 sep 2026 20:00:00 gmt").
func httpDate(s string) string {
	if len(s) < 3 {
		return s
	}
	return s[:len(s)-3] + "GMT"
}

// rfc3339 restores the "T" and "Z" time.Parse needs after a case-insensitive
// match ("2026-09-23 00:00:00z").
func rfc3339(s string) string {
	b := []byte(s)
	b[10] = 'T'
	if last := len(b) - 1; b[last] == 'z' {
		b[last] = 'Z'
	}
	return string(b)
}
