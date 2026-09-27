package main

// Styled Logs
//
// `harness logs` on a terminal, in the visual language of `harness list` and
// the cockpit: faint timestamps, the label column as coloured badges, lifecycle
// states as their SPEC-0003 glyph and colour, errors in the error colour, and a
// run of identical lines collapsed to one line with a count — a provider loop
// that logs the same error a thousand times reads as one line, not a thousand.
// The raw view re-renders the daemon's own charmbracelet/log lines the same way
// and leaves the agent's output between them exactly as it was.
//
// Styling is a terminal-only concern. A pipe, a file, --json or an agent
// reading stdout gets the plain renderers in logs.go and verbs.go byte for
// byte: a nil *logStyle selects them, and every styled path is keyed off one.
// NO_COLOR and degraded terminals are the theme's business — it resolves every
// colour through the detected colour profile, so under NO_COLOR the glyphs and
// words remain and the colour goes. The durable log on disk is untouched: the
// raw view parses newEventLogger's format read-only, and ReadLifecycle's
// parse of the same lines is the contract that format keeps.
//
// Governing: SPEC-0002 REQ "Control Operations" ("logs"), SPEC-0001 REQ
// "State Presentation" (paired glyph + colour, legible in mono), SPEC-0001 REQ
// "Zero And Error States" (one palette across cockpit and CLI), ADR-0001
// (Charmbracelet stack; lipgloss + the theme own the visual language),
// ADR-0007 (lifecycle events are charmbracelet/log lines in the durable log).
//
// @joestump-agent 09/26/2026 - Added: `harness logs` looked like crap on a
// terminal.
//
// @joestump-agent 09/27/2026 - The styling moved to internal/logview so the
// TUI renders one-shots and the chatroom the same way; this file keeps what is
// terminal-specific — in-place redraws under --follow, and split raw lines.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// logStyle renders the styled forms. A nil *logStyle means plain output.
type logStyle = logview.Style

var newLogStyle = logview.NewStyle

// logStyleFor is the style for output written to w: nil — plain — unless w is
// a terminal and --json is off, the same rule the tables follow.
func logStyleFor(w io.Writer) *logStyle {
	if !useColorFor(w) {
		return nil
	}
	return newLogStyle(theme.Default())
}

// --- the activity view -----------------------------------------------------

// activityView prints the activity view, plain or styled. The styled view
// collapses consecutive identical entries; a live one (--follow) does it in
// place, redrawing the open group's line as repeats arrive.
type activityView struct {
	w  io.Writer
	st *logStyle
	// live redraws the open group in place rather than printing it on flush.
	live bool
	// cols is the terminal width, for knowing how many rows a line took; 0
	// when unknown, which turns in-place redraws off.
	cols func() int
	grp  *logview.Group
	// rows is how many terminal rows the open group's line took, 0 when it
	// cannot be redrawn.
	rows int
}

func newActivityView(w io.Writer, st *logStyle) *activityView {
	return &activityView{w: w, st: st, cols: func() int { return termCols(w) }}
}

// termCols is w's terminal width, or 0.
func termCols(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if c, _, err := term.GetSize(int(f.Fd())); err == nil && c > 0 {
			return c
		}
	}
	return 0
}

func (v *activityView) header(r protocol.LogRun) {
	if v.st == nil {
		fmt.Fprintln(v.w, runHeader(r))
		return
	}
	v.close()
	fmt.Fprintln(v.w, v.st.RunHeader(r))
}

func (v *activityView) note(n string) {
	if v.st == nil {
		fmt.Fprintln(v.w, noteLine(n))
		return
	}
	v.close()
	fmt.Fprintln(v.w, v.st.NoteLine(n))
}

func (v *activityView) entry(e protocol.LogEntry) {
	if v.st == nil {
		fmt.Fprintln(v.w, formatEntry(e))
		return
	}
	if g := v.grp; g != nil && (!v.live || v.rows > 0) && g.Join(e) {
		if v.live {
			// Up to the first row of the group's line, clear to the end of
			// the screen, and draw it again with the new count.
			fmt.Fprintf(v.w, "\x1b[%dA\r\x1b[J", v.rows)
			v.draw()
		}
		return
	}
	v.close()
	v.grp = logview.NewGroup(e)
	if v.live {
		v.draw()
	}
}

// draw prints the open group's line and records how many rows it took.
func (v *activityView) draw() {
	line := v.st.GroupLine(v.grp)
	fmt.Fprintln(v.w, line)
	v.rows = rowsFor(line, v.cols())
}

// flush prints the open group, if the view has not already drawn it. A live
// view keeps its group open across polls, so a repeat in the next poll still
// joins it.
func (v *activityView) flush() {
	if v.live {
		return
	}
	v.close()
}

// close ends the open group, printing it unless it is already on screen.
func (v *activityView) close() {
	if v.grp != nil && !v.live {
		fmt.Fprintln(v.w, v.st.GroupLine(v.grp))
	}
	v.grp, v.rows = nil, 0
}

// rowsFor is how many terminal rows line takes at cols columns, or 0 when the
// width is unknown.
func rowsFor(line string, cols int) int {
	if cols <= 0 {
		return 0
	}
	w := lipgloss.Width(line)
	if w == 0 {
		return 1
	}
	return (w + cols - 1) / cols
}

// --- the raw view ------------------------------------------------------------

// rawWriter writes durable-log text, which arrives whole or — under --follow —
// as appended suffixes that can split a line. Styled, it re-renders each
// daemon line that starts at a line start and passes everything else through;
// plain, it is a plain write.
type rawWriter struct {
	w  io.Writer
	st *logStyle
	// mid is true when the last write ended inside a line, so the next one
	// starts with that line's continuation, not a line of its own.
	mid bool
}

func newRawWriter(w io.Writer, st *logStyle) *rawWriter { return &rawWriter{w: w, st: st} }

func (r *rawWriter) write(s string) {
	if s == "" {
		return
	}
	if r.st == nil {
		io.WriteString(r.w, s)
		return
	}
	var b strings.Builder
	for s != "" {
		seg, nl := s, false
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			seg, nl, s = s[:i], true, s[i+1:]
		} else {
			s = ""
		}
		if r.mid {
			b.WriteString(seg)
		} else {
			b.WriteString(r.st.RawLine(seg))
		}
		if nl {
			b.WriteByte('\n')
		}
		r.mid = !nl
	}
	io.WriteString(r.w, b.String())
}

// restart begins a reprinted tail on a line of its own. The plain view glues
// the reprint to whatever line it left open, and keeps doing so: a pipe reads
// the bytes it always has.
func (r *rawWriter) restart() {
	if r.st != nil && r.mid {
		io.WriteString(r.w, "\n")
		r.mid = false
	}
}
