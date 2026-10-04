package hours

// Governing: SPEC-0021 REQ-2 "The [budget] table" — day_starts' grammar, its
// zone prefix (shared with operating_hours through ParseZonePrefix) and the
// "A budget day in another zone" scenario at the parse level. The rollover
// itself is the admission story's (#470).
//
// @joestump 10/04/2026 - Added for #465.

import (
	"strings"
	"testing"
	"time"
)

func TestParseDailyInstantAccepts(t *testing.T) {
	cases := []struct {
		s        string
		zone     string // Location().String(); "Local" for no prefix
		hh, mm   int
		rendered string
	}{
		{"00:00", "Local", 0, 0, "00:00"},
		{"06:30", "Local", 6, 30, "06:30"},
		{"23:59", "Local", 23, 59, "23:59"},
		{"  07:15  ", "Local", 7, 15, "07:15"},
		{"TZ=America/Los_Angeles 06:00", "America/Los_Angeles", 6, 0, "TZ=America/Los_Angeles 06:00"},
		{"CRON_TZ=UTC 00:00", "UTC", 0, 0, "TZ=UTC 00:00"},
		{"TZ=Europe/Berlin\t08:30", "Europe/Berlin", 8, 30, "TZ=Europe/Berlin 08:30"},
	}
	for _, c := range cases {
		d, err := ParseDailyInstant(c.s)
		if err != nil {
			t.Errorf("ParseDailyInstant(%q): unexpected error: %v", c.s, err)
			continue
		}
		if got := d.Location().String(); got != c.zone {
			t.Errorf("ParseDailyInstant(%q).Location() = %q, want %q", c.s, got, c.zone)
		}
		if hh, mm := d.Clock(); hh != c.hh || mm != c.mm {
			t.Errorf("ParseDailyInstant(%q).Clock() = %02d:%02d, want %02d:%02d", c.s, hh, mm, c.hh, c.mm)
		}
		if got := d.String(); got != c.rendered {
			t.Errorf("ParseDailyInstant(%q).String() = %q, want %q", c.s, got, c.rendered)
		}
	}
}

func TestParseDailyInstantRejects(t *testing.T) {
	cases := []struct {
		s    string
		want string // substring the error must contain
	}{
		{"", "blank"},
		{"   ", "blank"},
		// SPEC-0021 REQ-2 "An unknown zone": the same error operating_hours
		// and schedule give, because it is the same parser.
		{"TZ=Mars/Olympus 00:00", `unknown time zone "Mars/Olympus"`},
		{"TZ= 00:00", "missing zone name"},
		{"TZ=UTC", "must name a time of day"},
		{"TZ=UTC 06:00 07:00", "malformed"},
		{"06:00 07:00", "malformed"},
		{"24:00", "cannot start at 24:00"},
		{"25:00", "out of range"},
		{"06:60", "out of range"},
		{"6:00", "malformed hour"},
		{"06:0", "malformed minute"},
		{"0600", "malformed"},
		{"Mon 06:00", "malformed"},
		{"06:00-07:00", "malformed"},
	}
	for _, c := range cases {
		_, err := ParseDailyInstant(c.s)
		if err == nil {
			t.Errorf("ParseDailyInstant(%q): expected an error containing %q, got nil", c.s, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseDailyInstant(%q) error = %q, want it to contain %q", c.s, err, c.want)
		}
	}
}

// TestDailyInstantZeroIsLocalMidnight pins SPEC-0021 REQ-2's default: an
// absent day_starts is 00:00 in the daemon's zone, which is what the zero
// value must mean, since config leaves the field zero when the key is absent.
func TestDailyInstantZeroIsLocalMidnight(t *testing.T) {
	var d DailyInstant
	if d.Location() != time.Local {
		t.Errorf("zero DailyInstant Location() = %v, want time.Local", d.Location())
	}
	if hh, mm := d.Clock(); hh != 0 || mm != 0 {
		t.Errorf("zero DailyInstant Clock() = %02d:%02d, want 00:00", hh, mm)
	}
	if d.String() != "00:00" {
		t.Errorf("zero DailyInstant String() = %q, want 00:00", d.String())
	}
}

// TestDailyInstantInAnotherZone is SPEC-0021 REQ-2 "A budget day in another
// zone" at the parse level: day_starts = "TZ=America/Los_Angeles 06:00" names
// 13:00 UTC on a summer day, whatever zone the daemon runs in.
func TestDailyInstantInAnotherZone(t *testing.T) {
	d, err := ParseDailyInstant("TZ=America/Los_Angeles 06:00")
	if err != nil {
		t.Fatal(err)
	}
	hh, mm := d.Clock()
	start := time.Date(2026, time.July, 15, hh, mm, 0, 0, d.Location())
	if want := time.Date(2026, time.July, 15, 13, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("summer day start = %v, want %v (13:00 UTC)", start.UTC(), want)
	}
	// And 14:00 UTC in winter, when Los Angeles is on PST: the zone is a
	// real zone, not a fixed offset captured at parse time.
	start = time.Date(2026, time.January, 15, hh, mm, 0, 0, d.Location())
	if want := time.Date(2026, time.January, 15, 14, 0, 0, 0, time.UTC); !start.Equal(want) {
		t.Errorf("winter day start = %v, want %v (14:00 UTC)", start.UTC(), want)
	}
}

// TestParseZonePrefix pins the exported prefix parser's contract: no prefix
// passes the input through, a prefix is split off and resolved.
func TestParseZonePrefix(t *testing.T) {
	zone, loc, rest, err := ParseZonePrefix("Mon-Fri 09:00-13:00")
	if err != nil || zone != "" || loc != nil || rest != "Mon-Fri 09:00-13:00" {
		t.Errorf("no prefix: got (%q, %v, %q, %v)", zone, loc, rest, err)
	}
	zone, loc, rest, err = ParseZonePrefix("CRON_TZ=UTC   06:00")
	if err != nil || zone != "UTC" || loc == nil || loc.String() != "UTC" || rest != "06:00" {
		t.Errorf("CRON_TZ=UTC: got (%q, %v, %q, %v)", zone, loc, rest, err)
	}
	zone, _, rest, err = ParseZonePrefix("TZ=Europe/London")
	if err != nil || zone != "Europe/London" || rest != "" {
		t.Errorf("bare prefix: got (%q, %q, %v)", zone, rest, err)
	}
	if _, _, _, err := ParseZonePrefix("TZ=Mars/Olympus 00:00"); err == nil ||
		!strings.Contains(err.Error(), `unknown time zone "Mars/Olympus"`) {
		t.Errorf("unknown zone: err = %v", err)
	}
}
