package attach

// Governing: SPEC-0002 REQ "Attach Session". x/vt tracks the guest's mouse
// reporting modes but exposes no reader for them, so the Mux shadows them from
// the EnableMode/DisableMode callbacks (as it does bracketed paste) and states
// them in every attach snapshot. The guest enables its mouse modes once, at
// startup; a snapshot only repaints cells, so without this a client that
// attaches later never learns the wheel belongs to the guest.

import (
	"fmt"

	"github.com/charmbracelet/x/ansi"
)

// mouseModeOrder is every mouse-reporting mode a client can act on: X10 (?9),
// normal (?1000), button-event (?1002), any-event (?1003) and the SGR encoding
// (?1006).
var mouseModeOrder = [...]ansi.DECMode{
	ansi.ModeMouseX10,
	ansi.ModeMouseNormal,
	ansi.ModeMouseButtonEvent,
	ansi.ModeMouseAnyEvent,
	ansi.ModeMouseExtSgr,
}

// mouseModes is the on/off state of mouseModeOrder, index for index.
type mouseModes [len(mouseModeOrder)]bool

// track records a mode change. Modes outside mouseModeOrder are ignored.
func (s *mouseModes) track(mode ansi.Mode, on bool) {
	for i, m := range mouseModeOrder {
		if mode == m {
			s[i] = on
			return
		}
	}
}

// repaint states every mouse mode, set or reset. Resets matter as much as sets:
// a client coalesced back to a snapshot may have missed the guest turning the
// mouse off, and would otherwise keep forwarding the wheel to a guest that no
// longer listens.
func (s mouseModes) repaint() []byte {
	var b []byte
	for i, m := range mouseModeOrder {
		final := byte('l')
		if s[i] {
			final = 'h'
		}
		b = fmt.Appendf(b, "\x1b[?%d%c", int(m), final)
	}
	return b
}
