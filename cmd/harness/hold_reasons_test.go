package main

// Off-Hours From hold_reasons
//
// `harness list` and `describe` read whether a harness is held for its hours
// from hold_reasons now that the projection's `held` is gone (SPEC-0021
// REQ-16): "hours" in the set renders off-hours with its next open, and a
// hold for any other reason alone does not.
//
// Governing: ADR-0027, SPEC-0021 REQ-16; ADR-0019, SPEC-0012 REQ "Operating
// Hours Visibility".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

func TestListRendersOffHoursFromHoldReasons(t *testing.T) {
	var buf bytes.Buffer
	hs := []protocol.HarnessInfo{
		{
			Name: "claude-src", State: "stopped", HoldReasons: []string{"hours", "quota"},
			OperatingHours: "Mon-Fri 09:00-13:00", HoursNext: "2026-09-28T09:00:00-07:00",
		},
		{
			Name: "parked-only", State: "stopped", HoldReasons: []string{"quota"},
			OperatingHours: "Mon-Fri 09:00-13:00", HoursNext: "2026-09-28T13:00:00-07:00",
		},
	}
	if err := printHarnessTable(&buf, hs); err != nil {
		t.Fatal(err)
	}
	var held, parked string
	for _, line := range strings.Split(buf.String(), "\n") {
		switch {
		case strings.Contains(line, "claude-src"):
			held = line
		case strings.Contains(line, "parked-only"):
			parked = line
		}
	}
	if !strings.Contains(held, "off-hours") || !strings.Contains(held, "opens Mon") || strings.Contains(held, "stopped") {
		t.Errorf("held for hours (and a park) = %q, want off-hours with its next open", held)
	}
	// Not held for its hours: no off-hours label and no "opens"; the parked
	// label is the listing story's (REQ-16), not this one's.
	if strings.Contains(parked, "off-hours") || strings.Contains(parked, "opens") {
		t.Errorf("held only for quota = %q, must not read off-hours", parked)
	}
}
