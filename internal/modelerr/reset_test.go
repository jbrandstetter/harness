package modelerr

// Reset-Time Parser Tests
//
// Governing tests: SPEC-0021 REQ-12 — a table test per reset shape in
// design.md "Reset-time formats", each with a provider's own message, the
// scenarios "A reset time in the message" and "A hostile reset time", and a
// fuzz test that ResetAfter never panics and never answers outside
// (now, now+8d]. REQ-11 "The phrase in a successful run" is pinned at the
// boundary below: the caller decides, this package only parses.
//
// @joestump 10/04/2026 - Added for harness#473.

import (
	"testing"
	"time"
	_ "time/tzdata" // named zones must not depend on the runner's zoneinfo
)

func zone(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

type resetCase struct {
	name    string
	note    string
	now     time.Time
	until   time.Time // zero: no reset
	clamped bool
}

func resetCases(t testing.TB) []resetCase {
	la := zone(t, "America/Los_Angeles")
	dublin := zone(t, "Europe/Dublin")
	utc := time.UTC
	at := func(loc *time.Location, y int, mo time.Month, d, h, mi, s int) time.Time {
		return time.Date(y, mo, d, h, mi, s, 0, loc)
	}
	noonish := at(utc, 2026, 9, 28, 19, 0, 0) // 12:00 in Los Angeles
	none := time.Time{}

	return []resetCase{
		// Claude Code -p: "<message>|<epoch seconds>".
		{"epoch suffix (REQ-11 one-shot scenario)", "Claude AI usage limit reached|1790000000",
			at(utc, 2026, 9, 21, 12, 13, 20), time.Unix(1790000000, 0), false},
		{"epoch suffix ending a line of a log tail", "Claude AI usage limit reached|1790000000\nexit status 1",
			at(utc, 2026, 9, 21, 12, 13, 20), time.Unix(1790000000, 0), false},
		{"epoch suffix already past", "Claude AI usage limit reached|1789430400",
			at(utc, 2026, 9, 21, 12, 0, 0), none, false},
		{"a hostile reset time (REQ-12 scenario)", "usage limit reached|99999999999",
			noonish, noonish.Add(8 * 24 * time.Hour), true},
		{"an epoch too big for int64 saturates into the clamp", "usage limit reached|999999999999999999999999",
			noonish, noonish.Add(8 * 24 * time.Hour), true},

		// Claude Code interactive and newer -p: "resets <h>[:mm]am|pm (<zone>)".
		{"a reset time in the message (REQ-12 scenario)", "5-hour limit reached ∙ resets 3pm (America/Los_Angeles)",
			at(la, 2026, 9, 22, 12, 40, 0), at(la, 2026, 9, 22, 15, 0, 0), false},
		{"the GH #15 / #782 error mark", "rate_limit (429): You've hit your session limit · resets 8:20pm (America/Los_Angeles)",
			at(la, 2026, 9, 28, 17, 5, 0), at(la, 2026, 9, 28, 20, 20, 0), false},
		{"a clock time already past today is tomorrow's", "rate_limit (429): You've hit your session limit · resets 8:20pm (America/Los_Angeles)",
			at(la, 2026, 9, 28, 21, 0, 0), at(la, 2026, 9, 29, 20, 20, 0), false},
		{"the zone in the message, not now's, places the clock time", "5-hour limit reached ∙ resets 3pm (America/Los_Angeles)",
			at(utc, 2026, 9, 22, 19, 40, 0), at(utc, 2026, 9, 22, 22, 0, 0), false},
		{"no zone: now's zone, the daemon's", "rate_limit (429): You've hit your session limit · resets 3pm",
			at(dublin, 2026, 9, 28, 16, 0, 0), at(dublin, 2026, 9, 29, 15, 0, 0), false},
		{"12am is midnight", "You've hit your session limit · resets 12am (UTC)",
			at(utc, 2026, 9, 28, 23, 30, 0), at(utc, 2026, 9, 29, 0, 0, 0), false},
		{"an unknown zone is not guessed at", "You've hit your session limit · resets 3pm (Mars/Olympus)",
			noonish, none, false},
		{"a weekday reset is not a clock time", "Too Many Requests: You have reached your weekly limit. It resets Monday 00:00 UTC.",
			noonish, none, false},

		// OpenAI, codex, litellm, Azure: "try again in <duration>", "retry after <n> seconds".
		{"OpenAI requests per day", "Rate limit reached for gpt-4o in organization org-abc123 on requests per day (RPD): Limit 10000, Used 10000, Requested 1. Please try again in 7m12s. Visit https://platform.openai.com/account/rate-limits to learn more.",
			noonish, noonish.Add(7*time.Minute + 12*time.Second), false},
		{"OpenAI tokens per minute is under the floor", "Rate limit reached for gpt-4o in organization org-abc123 on tokens per min (TPM): Limit 30000, Used 29123, Requested 1800. Please try again in 1.898s. Visit https://platform.openai.com/account/rate-limits to learn more.",
			noonish, none, false},
		{"the design's example", "Please try again in 2h13m",
			noonish, noonish.Add(2*time.Hour + 13*time.Minute), false},
		{"codex usage limit", "You've hit your usage limit. Upgrade to Pro or try again in 3 days 4 hours.",
			noonish, noonish.Add(76 * time.Hour), false},
		{"litellm router cooldown, exactly the floor", "litellm.RateLimitError: No deployments available for selected model, Try again in 60 seconds. Passed model=claude-sonnet-4. pre-call-checks=False, allowed_model_region=n/a",
			noonish, noonish.Add(time.Minute), false},
		{"just under the floor", "litellm.RateLimitError: No deployments available for selected model, Try again in 59 seconds.",
			noonish, none, false},
		{"Azure OpenAI via litellm", "Requests to the ChatCompletions_Create Operation under Azure OpenAI API version 2024-10-21 have exceeded token rate limit of your current OpenAI S0 pricing tier. Please retry after 86400 seconds. Please go here: https://aka.ms/oai/quotaincrease if you would like to further increase the default rate limit.",
			noonish, noonish.Add(24 * time.Hour), false},
		{"a hostile duration", "try again in 99999999999999999999999999 days",
			noonish, noonish.Add(8 * 24 * time.Hour), true},
		{"not a duration", "Please retry after 5 sessions have closed",
			noonish, none, false},

		// Gateways relaying the Retry-After header.
		{"Retry-After seconds in a relayed header block", "429 Too Many Requests\nretry-after: 300\nanthropic-ratelimit-requests-remaining: 0",
			noonish, noonish.Add(5 * time.Minute), false},
		{"Retry-After seconds in relayed JSON", `Too Many Requests: {"error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"headers":{"retry-after":"120"}}`,
			noonish, noonish.Add(2 * time.Minute), false},
		{"Retry-After as an HTTP date", "503 Service Unavailable\nRetry-After: Mon, 28 Sep 2026 20:00:00 GMT",
			noonish, at(utc, 2026, 9, 28, 20, 0, 0), false},
		{"retry-after-ms is not the seconds header", "retry-after-ms: 2000",
			noonish, none, false},

		// Generic: "reset(s) at <RFC 3339>".
		{"RFC 3339, the design's example", "quota resets at 2026-09-23T00:00:00Z",
			at(utc, 2026, 9, 22, 19, 40, 0), at(utc, 2026, 9, 23, 0, 0, 0), false},
		{"RFC 3339 with an offset", "Your monthly spend limit was reached; it will reset at 2026-10-01T00:00:00-07:00.",
			at(la, 2026, 9, 28, 12, 0, 0), at(la, 2026, 10, 1, 0, 0, 0), false},

		// OpenAI-compatible x-ratelimit-reset* headers.
		{"OpenAI token reset, a Go duration", "x-ratelimit-reset-tokens: 6m0s",
			noonish, noonish.Add(6 * time.Minute), false},
		{"Groq request reset, fractional", "x-ratelimit-reset-requests: 2m59.56s",
			noonish, noonish.Add(2*time.Minute + 59560*time.Millisecond), false},
		{"OpenRouter, epoch milliseconds in the error body", `{"error":{"message":"Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day","code":429,"metadata":{"headers":{"X-RateLimit-Limit":"50","X-RateLimit-Remaining":"0","X-RateLimit-Reset":"1790640000000"},"provider_name":null}},"user_id":"user_2abc"}`,
			noonish, at(utc, 2026, 9, 29, 0, 0, 0), false},
		{"epoch seconds", "x-ratelimit-reset: 1790640000",
			noonish, at(utc, 2026, 9, 29, 0, 0, 0), false},
		{"delta seconds", "x-ratelimit-reset: 3600",
			noonish, noonish.Add(time.Hour), false},

		// Several shapes: the earliest future instant, and no reset when that
		// one is under a minute away even if a later one is not.
		{"the earliest of several", "Claude AI usage limit reached|1790640000\nPlease try again in 2h13m",
			noonish, noonish.Add(2*time.Hour + 13*time.Minute), false},
		{"a past instant does not hide a future one", "quota resets at 2026-09-01T00:00:00Z; Please try again in 2h",
			noonish, noonish.Add(2 * time.Hour), false},
		{"the earliest is under the floor", "x-ratelimit-reset-requests: 1s\nx-ratelimit-reset-tokens: 6m0s",
			noonish, none, false},

		{"no reset in the note", "Bad Request: tool_use ids must be unique", noonish, none, false},
		{"an empty note", "", noonish, none, false},
	}
}

func TestResetAfter(t *testing.T) {
	for _, c := range resetCases(t) {
		until, clamped, ok := ResetAfter("claude-code", c.note, c.now)
		wantOK := !c.until.IsZero()
		if ok != wantOK || clamped != c.clamped || !until.Equal(c.until) {
			t.Errorf("%s: ResetAfter(%q, %s) = (%s, clamped %v, ok %v), want (%s, clamped %v, ok %v)",
				c.name, c.note, c.now, until, clamped, ok, c.until, c.clamped, wantOK)
		}
	}
}

// Every shape is one any adapter can relay, so the adapter does not change the
// answer.
func TestResetAfterReadsEveryShapeForEveryAdapter(t *testing.T) {
	for _, c := range resetCases(t) {
		want, wantClamped, wantOK := ResetAfter("claude-code", c.note, c.now)
		for _, adapter := range []string{"crush", "codex", "generic", ""} {
			got, clamped, ok := ResetAfter(adapter, c.note, c.now)
			if !got.Equal(want) || clamped != wantClamped || ok != wantOK {
				t.Errorf("%s: adapter %q = (%s, %v, %v), claude-code = (%s, %v, %v)", c.name, adapter, got, clamped, ok, want, wantClamped, wantOK)
			}
		}
	}
}

// SPEC-0021 REQ-11 "The phrase in a successful run": a run that exits 0 after
// printing "usage limit reached" classifies nothing and parks nothing. That is
// the caller's rule — the park detector (harness#477) never hands a successful
// run's output to this package. This package sees only a note, not an exit
// status, so the phrase parses here like any other; the guard cannot live here.
func TestThePhraseInASuccessfulRunIsTheCallersCall(t *testing.T) {
	note := "usage limit reached|1790000000"
	if c, known := Classify("claude-code", note); c != ClassQuota || !known {
		t.Errorf("Classify = (%s, %v), want quota", c, known)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	if _, _, ok := ResetAfter("claude-code", note, now); !ok {
		t.Error("ResetAfter found no reset: the note alone carries one")
	}
}

// FuzzResetAfter: whatever the note and whatever the clock, ResetAfter never
// panics, and a reset is in [now+1m, now+8d], equal to now+8d when clamped.
// No reset is the zero time, unclamped.
func FuzzResetAfter(f *testing.F) {
	for _, c := range resetCases(f) {
		f.Add("claude-code", c.note, int64(0))
	}
	for _, seed := range []string{
		"usage limit reached|9223372036854775807",
		"usage limit reached|-1",
		"resets 13pm (UTC)",
		"resets 0am",
		"resets 12:60pm (America/Los_Angeles)",
		"resets 3pm (../../etc/passwd)",
		"try again in 1.2.3s",
		"try again in .s",
		"try again in 1e308 seconds",
		"retry-after: 1e9",
		"Retry-After: Mon, 99 Sep 2026 20:00:00 GMT",
		"reset at 9999-12-31T23:59:59Z",
		"reset at 0000-01-01T00:00:00Z",
		"x-ratelimit-reset: 99999999999999999999999",
		"x-ratelimit-reset: 0",
		"x-ratelimit-reset: 1.5.5",
	} {
		f.Add("crush", seed, int64(0))
	}

	zones := []*time.Location{time.UTC, zone(f, "America/Los_Angeles"), zone(f, "Asia/Kolkata"), zone(f, "Pacific/Chatham")}
	base := time.Date(2026, 9, 28, 19, 0, 0, 0, time.UTC)
	const century = int64(100 * 365 * 24 * time.Hour)

	// The spec's numbers, not the package's constants, so a changed constant
	// fails here (SPEC-0021 REQ-12).
	const floor, clamp = time.Minute, 8 * 24 * time.Hour

	f.Fuzz(func(t *testing.T, adapter, note string, offset int64) {
		// now: within a century of the base, in one of a few zones (Chatham
		// is +12:45, so whole-hour assumptions break).
		now := base.Add(time.Duration(offset % century)).In(zones[uint64(offset)%uint64(len(zones))])
		until, clamped, ok := ResetAfter(adapter, note, now)
		if !ok {
			if !until.IsZero() || clamped {
				t.Fatalf("ResetAfter(%q) = (%s, clamped %v, false): no reset must be the zero time, unclamped", note, until, clamped)
			}
			return
		}
		if until.Before(now.Add(floor)) || until.After(now.Add(clamp)) {
			t.Fatalf("ResetAfter(%q) at %s = %s, outside [now+1m, now+8d]", note, now, until)
		}
		if clamped && !until.Equal(now.Add(clamp)) {
			t.Fatalf("ResetAfter(%q) at %s clamped to %s, want now+8d", note, now, until)
		}
	})
}
