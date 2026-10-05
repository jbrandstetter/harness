package tui

// Governing: SPEC-0001 REQ "Attached Mode". A guest that enabled mouse
// reporting (DECSET ?9/?1000/?1002/?1003) owns the mouse: a fullscreen agent
// TUI scrolls its own transcript from the wheel, so the client encodes the
// events the guest's mode asks for and forwards them to its PTY. tmux and
// screen draw the same line. Only a guest that has not asked for the mouse
// falls back to harness scrollback.

import (
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// guestMouse shadows the guest's mouse-reporting modes. x/vt tracks them but
// exposes no reader (the same reason vtView shadows DECTCEM), so they are
// rebuilt from the EnableMode/DisableMode callbacks. The daemon's snapshot
// states every one of them (internal/attach/mousemodes.go), so a client that
// attaches after the guest enabled them still learns them.
type guestMouse struct {
	x10, normal, button, anyEvent, sgr bool
}

// track records one mode change. Modes that are not mouse modes are ignored.
func (g *guestMouse) track(mode ansi.Mode, on bool) {
	switch mode {
	case ansi.ModeMouseX10:
		g.x10 = on
	case ansi.ModeMouseNormal:
		g.normal = on
	case ansi.ModeMouseButtonEvent:
		g.button = on
	case ansi.ModeMouseAnyEvent:
		g.anyEvent = on
	case ansi.ModeMouseExtSgr:
		g.sgr = on
	}
}

// reporting reports whether the guest asked for any mouse events at all.
func (g guestMouse) reporting() bool { return g.x10 || g.normal || g.button || g.anyEvent }

// encode returns the bytes the guest's PTY expects for msg, or nil when the
// guest's mode does not ask for this event or it falls outside the guest's
// cols x rows screen. When several modes are set the most verbose one wins, as
// in xterm.
func (g guestMouse) encode(msg tea.MouseMsg, cols, rows int) []byte {
	mo := msg.Mouse()
	if mo.X < 0 || mo.Y < 0 || mo.X >= cols || mo.Y >= rows {
		return nil
	}
	var motion, release, wheel bool
	switch msg.(type) {
	case tea.MouseClickMsg:
	case tea.MouseReleaseMsg:
		release = true
	case tea.MouseWheelMsg:
		wheel = true
	case tea.MouseMotionMsg:
		motion = true
	default:
		return nil
	}
	switch {
	case g.anyEvent:
	case g.button:
		if motion && mo.Button == tea.MouseNone {
			return nil
		}
	case g.normal:
		if motion {
			return nil
		}
	case g.x10:
		// X10 reports presses of the three buttons, with no modifiers.
		if motion || release || wheel {
			return nil
		}
		mo.Mod = 0
	default:
		return nil
	}
	b := ansi.EncodeMouseButton(mo.Button, motion,
		mo.Mod.Contains(tea.ModShift), mo.Mod.Contains(tea.ModAlt), mo.Mod.Contains(tea.ModCtrl))
	if g.sgr {
		return []byte(ansi.MouseSgr(b, mo.X, mo.Y, release))
	}
	// Legacy encoding: ESC [ M, then button, column and row each as one byte
	// offset by 32 (column and row 1-based), and a release as button 3. It
	// cannot address a cell past 223, so such events are dropped.
	if release {
		b = b&^0b11 | 3
	}
	const offset = 32
	if b == 0xff || mo.X+1+offset > 0xff || mo.Y+1+offset > 0xff || int(b)+offset > 0xff {
		return nil
	}
	return []byte{0x1b, '[', 'M', b + offset, byte(mo.X + 1 + offset), byte(mo.Y + 1 + offset)}
}

// guestOwnsMouse reports whether mouse events belong to the guest rather than
// to harness: it asked for mouse reporting and this client can drive it (a
// read-only viewer cannot, and keeps the harness scrollback). The frozen
// scrollback substate is explicitly harness's own.
func (m *Model) guestOwnsMouse() bool {
	a := m.att
	return m.mode == modeAttached && a != nil && m.attach != nil && a.substate == substateInteractive &&
		!a.readOnly() && a.view.mouse.reporting()
}
