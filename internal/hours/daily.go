package hours

// Daily Instant
//
// A DailyInstant is a time of day in a zone: SPEC-0021's [budget] day_starts,
// the instant each budget day begins. It shares the zone prefix and the HH:MM
// reading with operating_hours (ParseZonePrefix, parseTime), so the two keys
// cannot disagree on what a zone or a time means. Like Expr it is pure: it
// carries no clock, and the budget-day arithmetic that applies it to a
// caller-supplied instant belongs to internal/budget.
//
// Governing: SPEC-0021 REQ-2 "The [budget] table", REQ-3 "The budget day";
// ADR-0027; run-budgets design.md § "The budget day reuses the gate's clock",
// § "Config shapes".
//
// @joestump 10/04/2026 - Added for #465 (parse and validate only; nothing
// reads the instant until the admission story, #470).

import (
	"fmt"
	"strings"
	"time"
)

// DailyInstant is a parsed day_starts value. The zero DailyInstant is 00:00
// in the caller's zone, which is SPEC-0021 REQ-2's default for an absent
// day_starts; obtain any other from ParseDailyInstant.
type DailyInstant struct {
	// zoneText is the zone name as written after TZ=/CRON_TZ=, or "" when
	// the value carries no prefix (the daemon's local zone).
	zoneText string
	loc      *time.Location
	// secs is seconds since local midnight, in [0, 86400).
	secs int
}

// ParseDailyInstant validates and parses s against the day_starts grammar:
//
//	day_starts  = [ zone-prefix " " ] time
//	zone-prefix = ( "TZ=" / "CRON_TZ=" ) zone-name
//	time        = HH ":" MM          ; 00:00 through 23:59
//
// The zone prefix is ParseZonePrefix's, so an unknown zone fails with the
// same error operating_hours and schedule give. 24:00 is refused: a day that
// starts at the end of a day is 00:00, and two spellings for one instant
// would only make the round trip ambiguous. The caller (internal/config)
// wraps the error with the key and its line.
func ParseDailyInstant(s string) (DailyInstant, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return DailyInstant{}, fmt.Errorf("must not be blank")
	}
	zoneText, loc, rest, err := ParseZonePrefix(trimmed)
	if err != nil {
		return DailyInstant{}, err
	}
	switch fields := strings.Fields(rest); len(fields) {
	case 0:
		return DailyInstant{}, fmt.Errorf("must name a time of day (want \"[TZ=<zone> ]HH:MM\")")
	case 1:
		rest = fields[0]
	default:
		return DailyInstant{}, fmt.Errorf("%q: malformed (want \"[TZ=<zone> ]HH:MM\", one time of day)", rest)
	}
	if rest == "24:00" {
		return DailyInstant{}, fmt.Errorf("time %q: a day cannot start at 24:00 (use 00:00)", rest)
	}
	secs, err := parseTime(rest, false)
	if err != nil {
		return DailyInstant{}, err
	}
	return DailyInstant{zoneText: zoneText, loc: loc, secs: secs}, nil
}

// Location returns the zone the instant is read in: the prefix's zone, or
// time.Local for the zero DailyInstant and one written with no prefix.
func (d DailyInstant) Location() *time.Location {
	if d.loc != nil {
		return d.loc
	}
	return time.Local
}

// Clock returns the instant's wall-clock hour and minute in Location.
func (d DailyInstant) Clock() (hour, minute int) {
	return d.secs / 3600, (d.secs % 3600) / 60
}

// String renders d back into day_starts syntax, canonically: a CRON_TZ=
// prefix is written TZ=, as Expr.String does. ParseDailyInstant accepts the
// result and yields an equal instant.
func (d DailyInstant) String() string {
	if d.zoneText == "" {
		return fmtTime(d.secs)
	}
	return "TZ=" + d.zoneText + " " + fmtTime(d.secs)
}
