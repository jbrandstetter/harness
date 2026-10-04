package hours

// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Key" — design.md §
// Risks "Grammar surface": a new mini-language is a new parser to fuzz.
//
// @joestump-agent 09/21/2026 - Added for #381.

import (
	"testing"
	"time"
)

// FuzzParse round-trips Parse -> String -> Parse: whatever Parse accepts,
// String must render back into something Parse accepts again, and the two
// Exprs must agree on membership everywhere a week-long sample sweep checks —
// catching a grammar edge Parse silently mishandles well before it reaches a
// real harness.toml.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"09:00-13:00",
		"Mon-Fri 09:00-13:00",
		"TZ=America/Los_Angeles Mon-Fri 09:00-13:00",
		"CRON_TZ=UTC Mon-Fri 09:00-13:00",
		"Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00",
		"Sat,Sun 10:00-12:00",
		"Sun-Thu 22:00-02:00",
		"Mon-Sun 00:00-24:00",
		"Fri 22:00-02:00",
		"Fri-Mon 10:00-11:00",
		"Mon 09:00-09:00",
		"TZ=Mars/Olympus 09:00-13:00",
		"",
		"   ",
		"Xyz 09:00-13:00",
		"09:00-13:00-15:00",
		"24:00-13:00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		e1, err := Parse(s)
		if err != nil {
			return // s is not a valid expression; nothing to round-trip
		}

		s2 := e1.String()
		e2, err := Parse(s2)
		if err != nil {
			t.Fatalf("Parse(%q) succeeded, but Parse(String()) = Parse(%q) failed: %v", s, s2, err)
		}

		base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) // a Monday
		for i := 0; i < 7*24; i++ {
			sample := base.Add(time.Duration(i) * time.Hour)
			in1, _, _ := e1.In(sample)
			in2, _, _ := e2.In(sample)
			if in1 != in2 {
				t.Fatalf("round trip %q -> %q diverges at %v: %v != %v", s, s2, sample, in1, in2)
			}
		}

		// String() must itself be stable under a second round trip.
		s3 := e2.String()
		if s3 != s2 {
			t.Fatalf("String() not stable: %q -> %q -> %q", s, s2, s3)
		}
	})
}

// FuzzParseDailyInstant round-trips SPEC-0021's day_starts the same way
// FuzzParse round-trips operating_hours: whatever ParseDailyInstant accepts,
// String must render into something it accepts again, naming the same zone
// and wall-clock time, and String must be stable from there.
//
// @joestump 10/04/2026 - Added for #465, beside FuzzParse because day_starts
// shares its zone prefix and time reading.
func FuzzParseDailyInstant(f *testing.F) {
	for _, seed := range []string{
		"00:00",
		"06:30",
		"23:59",
		"TZ=America/Los_Angeles 06:00",
		"CRON_TZ=UTC 00:00",
		"TZ=Europe/Berlin\t08:30",
		"TZ=Mars/Olympus 00:00",
		"TZ= 00:00",
		"TZ=UTC",
		"24:00",
		"6:00",
		"06:00 07:00",
		"",
		"   ",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		d1, err := ParseDailyInstant(s)
		if err != nil {
			return // s is not a valid day_starts; nothing to round-trip
		}

		s2 := d1.String()
		d2, err := ParseDailyInstant(s2)
		if err != nil {
			t.Fatalf("ParseDailyInstant(%q) succeeded, but ParseDailyInstant(String()) = ParseDailyInstant(%q) failed: %v", s, s2, err)
		}
		if d1.Location().String() != d2.Location().String() {
			t.Fatalf("round trip %q -> %q changes the zone: %v != %v", s, s2, d1.Location(), d2.Location())
		}
		h1, m1 := d1.Clock()
		h2, m2 := d2.Clock()
		if h1 != h2 || m1 != m2 {
			t.Fatalf("round trip %q -> %q changes the time: %02d:%02d != %02d:%02d", s, s2, h1, m1, h2, m2)
		}
		if h1 < 0 || h1 > 23 || m1 < 0 || m1 > 59 {
			t.Fatalf("ParseDailyInstant(%q) accepted an out-of-range time %02d:%02d", s, h1, m1)
		}

		// String() must itself be stable under a second round trip.
		if s3 := d2.String(); s3 != s2 {
			t.Fatalf("String() not stable: %q -> %q -> %q", s, s2, s3)
		}
	})
}
