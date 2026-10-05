package tui

// Governing: SPEC-0001 (small cross-cutting helpers: PTY key encoding for
// attached forwarding, inline daemon start for the no-daemon zero-state, and
// TOML table deletion for the delete confirm — ADR-0006 file-is-truth).

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

// keyToBytes encodes a keystroke into the bytes a PTY expects, so attached
// interactive input forwards faithfully (SPEC-0001 scenario "Driving a live
// agent"). It covers the common control keys and arrow escapes; printable runes
// pass through verbatim.
func keyToBytes(msg tea.KeyPressMsg) []byte {
	// Modifiers are decided up front and applied uniformly (#178). Bubble Tea
	// v2 reports the modifier set as a bitmask: legacy terminals and tmux
	// collapse Ctrl+Shift+C into exactly ModCtrl, while Kitty-protocol
	// terminals (Ghostty with keyboard enhancements, flags=1) report the Shift
	// bit separately — so matching the mask for exact equality silently
	// swallowed the chord and the keystroke never reached the PTY.
	ctrl := msg.Mod&tea.ModCtrl != 0
	// Alt and Meta are indistinguishable on the wire (both are an ESC prefix
	// in the xterm/readline convention), so they are handled together.
	alt := msg.Mod&(tea.ModAlt|tea.ModMeta) != 0

	// Ctrl+A..Ctrl+Z map to 0x01..0x1a. Bubble Tea v2 dropped the KeyCtrlA..Z
	// key types and models Ctrl as a modifier on the base key. Match the Ctrl
	// BIT rather than the whole mask: Shift folds into the control code (a
	// terminal cannot express a shifted control code distinctly, and v1
	// behaved the same way by folding Shift into the key type), and Alt
	// prefixes ESC, which is what a terminal delivers for Alt+Ctrl chords.
	if ctrl && msg.Code >= 'a' && msg.Code <= 'z' {
		return altPrefix(alt, []byte{byte(msg.Code - 'a' + 1)})
	}
	// Shift and Ctrl on a cursor or editing key travel as xterm's modifier
	// parameter (ESC [ 1 ; 5 D for Ctrl+Left). Alt joins that parameter when
	// either is held; Alt alone keeps the ESC prefix below.
	shift := msg.Mod&tea.ModShift != 0
	xmod := 1
	if shift {
		xmod += 1
	}
	if alt {
		xmod += 2
	}
	if ctrl {
		xmod += 4
	}
	modified := shift || ctrl
	csi := func(num int, final byte) []byte {
		if !modified {
			if final == '~' {
				return []byte(fmt.Sprintf("\x1b[%d~", num))
			}
			return []byte{0x1b, '[', final}
		}
		if final == '~' {
			return []byte(fmt.Sprintf("\x1b[%d;%d~", num, xmod))
		}
		return []byte(fmt.Sprintf("\x1b[1;%d%c", xmod, final))
	}

	var base []byte
	switch msg.Code {
	case tea.KeyEnter:
		base = []byte{'\r'}
	case tea.KeyTab:
		if shift && !ctrl {
			// Back-tab. Claude Code cycles its permission mode on it.
			base = []byte("\x1b[Z")
		} else {
			base = []byte{'\t'}
		}
	case tea.KeyBackspace:
		base = []byte{0x7f}
	case tea.KeyDelete:
		base = csi(3, '~')
	case tea.KeyEscape:
		base = []byte{0x1b}
	case tea.KeyUp:
		base = csi(1, 'A')
	case tea.KeyDown:
		base = csi(1, 'B')
	case tea.KeyRight:
		base = csi(1, 'C')
	case tea.KeyLeft:
		base = csi(1, 'D')
	case tea.KeyHome:
		base = csi(1, 'H')
	case tea.KeyEnd:
		base = csi(1, 'F')
	case tea.KeyPgUp:
		base = csi(5, '~')
	case tea.KeyPgDown:
		base = csi(6, '~')
	}
	if base == nil && msg.Text != "" {
		// Printable characters (space included) pass through verbatim. Text is
		// populated only for keys that produce printable output, so this is
		// v1's KeyRunes/KeySpace pair collapsed into one case.
		base = []byte(msg.Text)
	}
	if modified && isCSIKey(msg.Code) {
		return base // Alt is already in the modifier parameter
	}
	// Alt/Meta prefixes ESC — what a real terminal sends for an Alt chord
	// (#178: this used to forward Alt+a as a bare "a", losing the modifier).
	return altPrefix(alt, base)
}

func isCSIKey(code rune) bool {
	switch code { //nolint:exhaustive
	case tea.KeyUp, tea.KeyDown, tea.KeyRight, tea.KeyLeft, tea.KeyHome, tea.KeyEnd,
		tea.KeyDelete, tea.KeyPgUp, tea.KeyPgDown:
		return true
	}
	return false
}

// altPrefix prepends the ESC byte an Alt/Meta chord carries on the wire.
func altPrefix(alt bool, b []byte) []byte {
	if !alt || len(b) == 0 {
		return b
	}
	return append([]byte{0x1b}, b...)
}

// textinputBlink is the textinput cursor-blink Cmd.
func textinputBlink() tea.Cmd { return textinput.Blink }

// splitLines splits text into display lines, dropping a single trailing empty
// line so a final newline doesn't add a blank row.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// startDaemonCmd launches `harness daemon` in the background for the no-daemon
// inline offer (SPEC-0001 scenario "Daemon not running"), then runs `then` (a
// redial).
func startDaemonCmd(opts Options, then tea.Cmd) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			cmd := exec.Command("harness", "daemon", "--socket", opts.Socket, "--config", opts.ConfigPath)
			cmd.Stdout, cmd.Stderr = nil, nil
			_ = cmd.Start()
			time.Sleep(600 * time.Millisecond) // give it a moment to bind the socket
			return nil
		},
		then,
	)
}

// deleteHarnessCmd removes a harness table from harness.toml and reloads the
// daemon (ADR-0006). If the file can't be read it surfaces the error via the
// reload result.
func (m *Model) deleteHarnessCmd(name string) tea.Cmd {
	path := m.opts.ConfigPath
	ctrl := m.ctrl
	return func() tea.Msg {
		body, err := os.ReadFile(path)
		if err != nil {
			return reloadResultMsg{err: err}
		}
		out := removeHarnessTOML(string(body), name)
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			return reloadResultMsg{err: err}
		}
		hs, rerr := ctrl.Reload()
		return reloadResultMsg{harnesses: hs, err: rerr}
	}
}

// tableHeaderRe matches any TOML table header line.
var tableHeaderRe = regexp.MustCompile(`^\s*\[`)

// removeHarnessTOML drops the [harness.<name>] (or bare [<name>]) table and its
// body from a harness.toml source, up to the next table header or EOF. It is a
// line-oriented edit that preserves the rest of the file (ADR-0006).
func removeHarnessTOML(body, name string) string {
	lines := strings.Split(body, "\n")
	want := map[string]bool{
		"[harness." + name + "]": true,
		"[" + name + "]":         true,
	}
	var out []string
	skipping := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if tableHeaderRe.MatchString(ln) {
			// A new table header ends any skip and decides whether to start one.
			skipping = want[trimmed]
			if skipping {
				continue
			}
		}
		if skipping {
			continue
		}
		out = append(out, ln)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}
