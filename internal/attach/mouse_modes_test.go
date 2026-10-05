package attach

// Governing: SPEC-0002 REQ "Attach Session" (the snapshot is "correct before
// any live bytes arrive"). A snapshot repaints cells, so it cannot say that the
// guest asked for mouse reporting; the guest enabled it once, at startup, and
// never repeats it. A client that attaches later (or is coalesced back to a
// snapshot) therefore has to be told the mouse modes explicitly, or it cannot
// know the wheel belongs to the guest.

import (
	"bytes"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// firstFrame attaches a fresh session and returns its snapshot frame.
func firstFrame(t *testing.T, m *Mux) []byte {
	t.Helper()
	c := &collector{}
	s := m.Attach(1, protocol.AttachRW, 80, 24, c.write)
	t.Cleanup(func() { m.Detach(s) })
	waitForFrameContaining(t, c, snapshotPrefix)
	return c.all()[0]
}

func TestSnapshotCarriesGuestMouseModes(t *testing.T) {
	cases := []struct {
		name     string
		guestOut string
		want     []string
		notWant  []string
	}{
		{
			name:     "fullscreen agent: any-event tracking with SGR encoding",
			guestOut: "\x1b[?1003h\x1b[?1006h",
			want:     []string{"\x1b[?1003h", "\x1b[?1006h"},
			notWant:  []string{"\x1b[?1000h", "\x1b[?1002h"},
		},
		{
			name:     "button-event tracking",
			guestOut: "\x1b[?1000h\x1b[?1002h\x1b[?1006h",
			want:     []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h"},
			notWant:  []string{"\x1b[?1003h"},
		},
		{
			name:     "guest that never asked for the mouse repaints every mouse mode as off",
			guestOut: "$ ",
			want:     []string{"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1006l"},
			notWant:  []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1006h"},
		},
		{
			// A snapshot that only ever said "on" would leave a client that
			// missed the guest's later reset believing the mouse is still the
			// guest's, so the wheel would go nowhere.
			name:     "guest that turned mouse reporting back off",
			guestOut: "\x1b[?1002h\x1b[?1006h\x1b[?1002l\x1b[?1006l",
			want:     []string{"\x1b[?1002l", "\x1b[?1006l"},
			notWant:  []string{"\x1b[?1002h", "\x1b[?1006h"},
		},
		{
			name:     "full terminal reset drops the guest's mouse modes",
			guestOut: "\x1b[?1002h\x1b[?1006h\x1bc",
			want:     []string{"\x1b[?1002l", "\x1b[?1006l"},
			notWant:  []string{"\x1b[?1002h", "\x1b[?1006h"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMux("h", 100, nil, nil, nil)
			m.Write([]byte(tc.guestOut))
			snap := firstFrame(t, m)
			for _, w := range tc.want {
				if !bytes.Contains(snap, []byte(w)) {
					t.Errorf("snapshot lacks %q: %q", w, snap)
				}
			}
			for _, w := range tc.notWant {
				if bytes.Contains(snap, []byte(w)) {
					t.Errorf("snapshot must not contain %q: %q", w, snap)
				}
			}
		})
	}
}

// The capture path hands the snapshot's bytes to a real terminal; enabling
// mouse reporting there would leave the caller's terminal grabbing the wheel.
func TestAnsiScreenStaysFreeOfModes(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	m.Write([]byte("\x1b[?1003h\x1b[?1006h"))
	if out := m.AnsiScreen(); bytes.Contains(out, []byte("\x1b[?")) {
		t.Fatalf("AnsiScreen must repaint cells only, got %q", out)
	}
}

// A client that fell behind is repainted from renderSnapshotLocked instead of
// the backlog; that repaint must carry the modes too.
func TestCoalescedSnapshotCarriesGuestMouseModes(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	m.Write([]byte("\x1b[?1003h\x1b[?1006h"))
	m.mu.Lock()
	snap := m.renderSnapshotLocked()
	m.mu.Unlock()
	for _, w := range []string{"\x1b[?1003h", "\x1b[?1006h"} {
		if !bytes.Contains(snap, []byte(w)) {
			t.Errorf("coalesced snapshot lacks %q: %q", w, snap)
		}
	}
}
