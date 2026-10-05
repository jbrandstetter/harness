package tui

// Governing: SPEC-0001 REQ "Attached Mode" ("every other keystroke forwards
// straight to the harness PTY") and REQ "Scrollback Substate". A guest that
// turned on mouse reporting (DECSET ?1000/?1002/?1003) owns the wheel: a
// fullscreen agent TUI such as Claude Code scrolls its own transcript from it.
// The client forwards the events instead of opening its own scrollback, which
// for such a guest holds nothing but repaint shards. A guest that never asked
// for the mouse keeps the harness scrollback.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/protocol"
)

// guestMouseFixture is an attached read-write model whose guest has emitted
// guestOut, exactly as the live ATTACH_DATA stream would deliver it.
func guestMouseFixture(t *testing.T, guestOut string) attachedFixture {
	t.Helper()
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	fx.m.att.view.write([]byte(guestOut))
	return fx
}

func at(msg tea.MouseMsg, x, y int) tea.MouseMsg {
	switch e := msg.(type) {
	case tea.MouseWheelMsg:
		e.X, e.Y = x, y
		return e
	case tea.MouseClickMsg:
		e.X, e.Y = x, y
		return e
	case tea.MouseReleaseMsg:
		e.X, e.Y = x, y
		return e
	case tea.MouseMotionMsg:
		e.X, e.Y = x, y
		return e
	}
	return msg
}

func sentToGuest(fa *fakeAttach) []string {
	out := make([]string, len(fa.inputs))
	for i, b := range fa.inputs {
		out[i] = string(b)
	}
	return out
}

func TestWheelReachesGuestThatEnabledMouseReporting(t *testing.T) {
	const sgrAnyEvent = "\x1b[?1003h\x1b[?1006h" // what a fullscreen Claude Code asks for
	fx := guestMouseFixture(t, sgrAnyEvent)
	m, fa := fx.m, fx.fa

	_, cmd := m.onMouse(at(wheelUp(), 49, 14))
	drain(cmd)
	if m.att.substate != substateInteractive {
		t.Fatal("wheel-up over a guest that owns the mouse must not open harness scrollback")
	}
	_, cmd = m.onMouse(at(tea.MouseWheelMsg{Button: tea.MouseWheelDown}, 49, 14))
	drain(cmd)

	got := sentToGuest(fa)
	want := []string{"\x1b[<64;50;15M", "\x1b[<65;50;15M"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("guest received %q, want %q", got, want)
	}
}

func TestWheelOpensScrollbackWhenGuestIgnoresMouse(t *testing.T) {
	fx := guestMouseFixture(t, "$ ls\r\n")
	m, fa := fx.m, fx.fa

	_, cmd := m.onMouse(at(wheelUp(), 49, 14))
	drain(cmd)
	if m.att.substate != substateScrollback {
		t.Fatal("a guest that never enabled mouse reporting keeps harness scrollback on wheel-up")
	}
	if len(fa.inputs) != 0 {
		t.Fatalf("nothing may reach a guest that did not ask for mouse events, got %q", sentToGuest(fa))
	}
}

func TestMouseOwnershipFollowsGuestModes(t *testing.T) {
	steps := []struct {
		name     string
		guestOut string
		owns     bool
	}{
		{"enabled", "\x1b[?1002h\x1b[?1006h", true},
		{"still on after unrelated output", "hello\r\n", true},
		{"disabled", "\x1b[?1002l\x1b[?1006l", false},
		{"re-enabled", "\x1b[?1000h", true},
		{"full reset", "\x1bc", false},
	}
	fx := guestMouseFixture(t, "")
	for _, st := range steps {
		fx.m.att.view.write([]byte(st.guestOut))
		fx.m.att.substate = substateInteractive
		fx.fa.inputs = nil
		_, cmd := fx.m.onMouse(at(wheelUp(), 3, 3))
		drain(cmd)
		if got := len(fx.fa.inputs) == 1; got != st.owns {
			t.Fatalf("%s: forwarded=%v, want %v (substate %v)", st.name, got, st.owns, fx.m.att.substate)
		}
		if st.owns && fx.m.att.substate != substateInteractive {
			t.Fatalf("%s: guest owns the mouse yet scrollback opened", st.name)
		}
		if !st.owns && fx.m.att.substate != substateScrollback {
			t.Fatalf("%s: guest released the mouse yet scrollback did not open", st.name)
		}
	}
}

func TestMouseEventsFollowWhatTheModeAsksFor(t *testing.T) {
	press := click()
	drag := tea.MouseMotionMsg{Button: tea.MouseLeft}
	hover := tea.MouseMotionMsg{Button: tea.MouseNone}
	release := tea.MouseReleaseMsg{Button: tea.MouseLeft}
	cases := []struct {
		name     string
		guestOut string
		msg      tea.MouseMsg
		want     string // "" = must not be forwarded
	}{
		{"normal: press", "\x1b[?1000h\x1b[?1006h", press, "\x1b[<0;11;6M"},
		{"normal: release", "\x1b[?1000h\x1b[?1006h", release, "\x1b[<0;11;6m"},
		{"normal: no drag", "\x1b[?1000h\x1b[?1006h", drag, ""},
		{"button-event: drag", "\x1b[?1002h\x1b[?1006h", drag, "\x1b[<32;11;6M"},
		{"button-event: no hover", "\x1b[?1002h\x1b[?1006h", hover, ""},
		{"any-event: hover", "\x1b[?1003h\x1b[?1006h", hover, "\x1b[<35;11;6M"},
		{"modifiers", "\x1b[?1000h\x1b[?1006h", tea.MouseWheelMsg{Button: tea.MouseWheelUp, Mod: tea.ModCtrl}, "\x1b[<80;11;6M"},
		// Without ?1006 the wire format is the legacy one: ESC [ M, then
		// button, column and row each offset by 32 (column and row 1-based).
		{"legacy encoding: press", "\x1b[?1000h", press, "\x1b[M" + string([]byte{32, 32 + 11, 32 + 6})},
		{"legacy encoding: release", "\x1b[?1000h", release, "\x1b[M" + string([]byte{32 + 3, 32 + 11, 32 + 6})},
		// X10 mode reports presses only.
		{"x10: press", "\x1b[?9h", press, "\x1b[M" + string([]byte{32, 32 + 11, 32 + 6})},
		{"x10: release", "\x1b[?9h", release, ""},
		{"x10: wheel", "\x1b[?9h", wheelUp(), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := guestMouseFixture(t, tc.guestOut)
			_, cmd := fx.m.onMouse(at(tc.msg, 10, 5))
			drain(cmd)
			got := sentToGuest(fx.fa)
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("must not be forwarded, got %q", got)
			case tc.want != "" && (len(got) != 1 || got[0] != tc.want):
				t.Fatalf("guest received %q, want %q", got, tc.want)
			}
			if fx.m.att.substate != substateInteractive {
				t.Fatal("a guest that owns the mouse never gets harness scrollback from mouse events")
			}
		})
	}
}

func TestMouseOutsideGuestScreenIsDropped(t *testing.T) {
	fx := guestMouseFixture(t, "\x1b[?1002h\x1b[?1006h")
	// The fixture's terminal is 80x24; row 24 is the status bar.
	for _, p := range [][2]int{{10, 24}, {80, 5}, {-1, 5}, {5, -1}} {
		_, cmd := fx.m.onMouse(at(wheelUp(), p[0], p[1]))
		drain(cmd)
	}
	if got := sentToGuest(fx.fa); len(got) != 0 {
		t.Fatalf("events outside the guest's screen must not reach it, got %q", got)
	}
	if fx.m.att.substate != substateInteractive {
		t.Fatal("events outside the guest's screen must not open scrollback either")
	}
}

func TestReadOnlyAttachKeepsScrollbackAndForwardsNothing(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRO)
	fx.m.att.view.write([]byte("\x1b[?1003h\x1b[?1006h"))
	_, cmd := fx.m.onMouse(at(wheelUp(), 3, 3))
	drain(cmd)
	if len(fx.fa.inputs) != 0 {
		t.Fatalf("a read-only attach must not send input, got %q", sentToGuest(fx.fa))
	}
	if fx.m.att.substate != substateScrollback {
		t.Fatal("a read-only viewer cannot drive the guest, so the wheel keeps its scrollback")
	}
}

func TestShiftClickStillReleasesGrabOverMouseGuest(t *testing.T) {
	fx := guestMouseFixture(t, "\x1b[?1002h\x1b[?1006h")
	_, cmd := fx.m.onMouse(shiftClick())
	_ = cmd
	if !fx.m.mouseReleased {
		t.Fatal("shift+click is the escape hatch for native selection and must keep working")
	}
	if len(fx.fa.inputs) != 0 {
		t.Fatalf("the selection gesture must not reach the guest, got %q", sentToGuest(fx.fa))
	}
}

func TestScrollbackSubstateKeepsItsOwnWheel(t *testing.T) {
	fx := guestMouseFixture(t, "\x1b[?1002h\x1b[?1006h")
	fx.m.att.enterScrollback(fx.m.peekLines(), fx.m.scrollbackHeight())
	_, cmd := fx.m.onMouse(at(wheelUp(), 3, 3))
	drain(cmd)
	if len(fx.fa.inputs) != 0 {
		t.Fatalf("inside the frozen scrollback the wheel navigates it, got %q", sentToGuest(fx.fa))
	}
}

func TestAnyEventGuestAsksTerminalForAllMotion(t *testing.T) {
	for _, tc := range []struct {
		guestOut string
		want     tea.MouseMode
	}{
		{"", tea.MouseModeCellMotion},
		{"\x1b[?1002h\x1b[?1006h", tea.MouseModeCellMotion},
		{"\x1b[?1003h\x1b[?1006h", tea.MouseModeAllMotion},
	} {
		fx := guestMouseFixture(t, tc.guestOut)
		if got := fx.m.View().MouseMode; got != tc.want {
			t.Errorf("guest %q: MouseMode=%v, want %v", tc.guestOut, got, tc.want)
		}
	}
	// A shift gesture still hands the mouse back whatever the guest asked for.
	fx := guestMouseFixture(t, "\x1b[?1003h\x1b[?1006h")
	_, _ = fx.m.onMouse(shiftClick())
	if fx.m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("shift release must drop mouse reporting even for an any-event guest")
	}
}

func TestPageUpReachesGuestThatOwnsTheMouse(t *testing.T) {
	fx := guestMouseFixture(t, "\x1b[?1003h\x1b[?1006h")
	_, cmd := fx.m.onKey(specialKey(tea.KeyPgUp))
	drain(cmd)
	if fx.m.att.substate != substateInteractive {
		t.Fatal("PgUp must not open harness scrollback over a guest that scrolls itself")
	}
	if got := sentToGuest(fx.fa); len(got) != 1 || got[0] != "\x1b[5~" {
		t.Fatalf("guest received %q, want PgUp", got)
	}
	// ^b [ stays the explicit way into the harness scrollback.
	_, _ = fx.m.onKey(ctrlKey('b'))
	_, _ = fx.m.onKey(runeKey("["))
	if fx.m.att.substate != substateScrollback {
		t.Fatal("^b [ must still open harness scrollback")
	}
}

func TestPageUpOpensScrollbackForPlainGuest(t *testing.T) {
	fx := guestMouseFixture(t, "$ ")
	_, cmd := fx.m.onKey(specialKey(tea.KeyPgUp))
	drain(cmd)
	if fx.m.att.substate != substateScrollback || len(fx.fa.inputs) != 0 {
		t.Fatalf("PgUp on a guest without mouse reporting keeps opening scrollback (substate %v, sent %q)", fx.m.att.substate, sentToGuest(fx.fa))
	}
}

func TestOverlayKeepsMouseAwayFromGuest(t *testing.T) {
	fx := guestMouseFixture(t, "\x1b[?1002h\x1b[?1006h")
	fx.m.overlay = overlayHelp
	_, cmd := fx.m.onMouse(at(wheelUp(), 3, 3))
	drain(cmd)
	if len(fx.fa.inputs) != 0 || fx.m.att.substate != substateInteractive {
		t.Fatalf("an open overlay must neither forward to the guest nor open scrollback (sent %q, substate %v)", sentToGuest(fx.fa), fx.m.att.substate)
	}
}
