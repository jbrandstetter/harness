package daemon

// Hold Reasons On The Wire
//
// SPEC-0021 REQ-16 "No held field" and the event half of REQ-19, asserted on
// the bytes the daemon writes to its socket rather than on a decoded struct:
// a struct with no Held field cannot show that the JSON lacks one, and the
// point of the change is the wire. The projection of a harness held for its
// hours carries hold_reasons ["hours"] and no held key; one that is not held
// carries neither. Every change of the set reaches a subscriber as
// harness_hold_changed { name, hold_reasons, next }.
//
// Governing: ADR-0027, SPEC-0021 REQ-16, REQ-19; SPEC-0002 REQ "Control
// Operations", REQ "Event Subscription".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

const holdsTOML = `
[harness.gated]
harness = "generic"
args = ["-c", "while true; do sleep 0.2; done"]
operating_hours = "Mon 09:00-13:00"
hours_shutdown = "immediate"

[harness.plain]
harness = "generic"
args = ["-c", "while true; do sleep 0.2; done"]
`

// rawControl sends one control request and returns the reply's data exactly
// as the daemon encoded it.
func rawControl(t *testing.T, c *client.Client, req protocol.ControlReq) json.RawMessage {
	t.Helper()
	pc := c.Conn()
	req.ID = 9000
	if err := pc.WriteJSON(protocol.TypeControlReq, &req); err != nil {
		t.Fatalf("write %s: %v", req.Op, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("read %s reply: %v", req.Op, err)
		}
		switch f.Type {
		case protocol.TypeControlResp:
			var resp protocol.ControlResp
			if err := json.Unmarshal(f.Payload, &resp); err != nil {
				t.Fatalf("decode %s reply: %v", req.Op, err)
			}
			if resp.ID == req.ID {
				return resp.Data
			}
		case protocol.TypeError:
			t.Fatalf("%s: error frame %s", req.Op, f.Payload)
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		}
	}
}

// fieldsOf decodes one harness projection into its raw keys.
func fieldsOf(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode projection: %v (%s)", err, raw)
	}
	return m
}

// SPEC-0021 REQ-16 Scenario "No held field", through describe and list.
func TestProjectionCarriesHoldReasonsAndNoHeld(t *testing.T) {
	td := newTestDaemon(t, holdsTOML)
	td.mgr.Start("gated")
	td.mgr.Start("plain")
	waitFor(t, "both running", func() bool {
		g, _ := td.mgr.Snapshot("gated")
		p, _ := td.mgr.Snapshot("plain")
		return g.State == core.StateRunning && p.State == core.StateRunning
	})
	td.mgr.Hold("gated", core.HoldHours, core.HoursShutdownImmediate, time.Time{})
	c := td.dial(t, nil)

	described := fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "gated"}))
	if got := string(described["hold_reasons"]); got != `["hours"]` {
		t.Errorf(`describe hold_reasons = %s, want ["hours"]`, got)
	}
	if v, ok := described["held"]; ok {
		t.Errorf("describe still carries held: %s", v)
	}

	var listed []json.RawMessage
	if err := json.Unmarshal(rawControl(t, c, protocol.ControlReq{Op: protocol.OpList}), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	seen := 0
	for _, raw := range listed {
		f := fieldsOf(t, raw)
		if v, ok := f["held"]; ok {
			t.Errorf("list carries held for %s: %s", f["name"], v)
		}
		switch string(f["name"]) {
		case `"gated"`:
			seen++
			if got := string(f["hold_reasons"]); got != `["hours"]` {
				t.Errorf(`list hold_reasons for gated = %s, want ["hours"]`, got)
			}
		case `"plain"`:
			seen++
			if v, ok := f["hold_reasons"]; ok {
				t.Errorf("a harness that is not held carries hold_reasons: %s", v)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("list returned %d of the two harnesses: %s", seen, listed)
	}
}

// SPEC-0021 REQ-19, event half, over the socket: a hold and the stop that
// clears it each reach a subscriber as harness_hold_changed.
func TestHoldChangedReachesSubscribers(t *testing.T) {
	td := newTestDaemon(t, holdsTOML)
	td.mgr.Start("gated")
	waitFor(t, "running", func() bool { s, _ := td.mgr.Snapshot("gated"); return s.State == core.StateRunning })
	sub := td.dial(t, []string{"events"})
	pc := sub.Conn()
	// The read deadline starts when the read does, not before the action
	// that triggers it: Hold blocks through the harness's stop, and where
	// the process ignores SIGTERM (main's CI runner) that is the whole
	// 10s default stop grace, which would expire a deadline set earlier
	// before the already-delivered event is read.
	next := func() (protocol.EventMsg, string) {
		t.Helper()
		_ = sub.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			f, err := pc.ReadFrame()
			if err != nil {
				t.Fatalf("waiting for harness_hold_changed: %v", err)
			}
			switch f.Type {
			case protocol.TypeEvent:
				if ev := decodeEvent(t, f.Payload); ev.Kind == protocol.EvHoldChanged && ev.Name == "gated" {
					return ev, string(f.Payload)
				}
			case protocol.TypePing:
				_ = pc.WriteFrame(protocol.TypePong, nil)
			}
		}
	}

	td.mgr.Hold("gated", core.HoldHours, core.HoursShutdownImmediate, time.Time{})
	held, raw := next()
	if !strings.Contains(raw, `"kind":"harness_hold_changed"`) || !strings.Contains(raw, `"hold_reasons":["hours"]`) || !strings.Contains(raw, `"next":"`) {
		t.Errorf("hold event = %s, want harness_hold_changed with hold_reasons [hours] and next", raw)
	}
	if _, err := time.Parse(time.RFC3339, held.HoldNext); err != nil {
		t.Errorf("hold event next = %q, want the next open (RFC 3339): %v", held.HoldNext, err)
	}

	td.mgr.Stop("gated")
	_, raw = next()
	if strings.Contains(raw, `"hold_reasons"`) || strings.Contains(raw, `"next"`) {
		t.Errorf("event after stop = %s, want no hold reasons and no next", raw)
	}
}
