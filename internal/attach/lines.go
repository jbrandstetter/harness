package attach

// Line Mode: Attach Without A Terminal
//
// A structured one-shot (claude-code's stream-json) runs on pipes, with no PTY
// (ADR-0033 "Structured one-shots run on pipes"; internal/supervisor,
// pipes.go). Its output is lines, not a screen, and a Mux would spend an x/vt
// emulator rendering them into one: a screen nobody attaches to, with a
// scrollback that grows with every line. The daemon ran to 12 GB on such runs
// (https://github.com/stump-wtf/harness/issues/18).
//
// So such a harness is served by a LineMux instead, which has no emulator:
//
//   - its snapshot is the recent lines, replayed from the same byte-bounded
//     ring a Mux replays scrollback from (ring.go), with no screen repaint in
//     front of them;
//   - live output is fanned out to the sessions as it arrives, in frames no
//     larger than lineFrame, so a multi-megabyte line cannot fill a slow
//     session's queue with megabytes;
//   - a session too slow to keep up has its backlog replaced by a one-line
//     notice rather than a repaint, since there is no screen to repaint;
//   - there is no viewport to negotiate and no stdin: resize is recorded for
//     describe and changes nothing, and input is dropped.
//
// The wire does not change. A client gets ATTACH_DATA bytes either way: here,
// the CRLF-terminated lines a terminal would have shown, which the TUI's
// stream-json peek formatter and `harness attach` already read.
//
// Which mux serves a harness is decided by what its runs actually do. The
// Registry hands the supervisor one Output per harness. A PTY run's bytes
// arrive through Output.Write and go to the Mux; a pipe run asks for
// Output.LineOut and writes the LineMux. Either records the harness's mode, and
// an attach goes to the mux of the latest run. Before any run it goes by the
// harness's definition (AttachTarget). A terminal harness's Mux is built with
// its supervisor, as it always was (Output.Prime); a stream-json harness's
// LineMux waits for its first run or attach, and it never builds an emulator.
//
// Governing: ADR-0033 "Structured one-shots run on pipes"; SPEC-0002 REQ
// "Attach Session" and REQ "Backpressure Isolation" (the reader never blocks
// on a client; bounded per-session queues); SPEC-0017 REQ-18; ADR-0007
// (byte-bounded ring).
//
// @joestump-agent 09/28/2026 - Added for
// https://github.com/stump-wtf/harness/issues/18.

import (
	"io"
	"sort"
	"sync"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/harness/internal/protocol"
)

// newEmulator builds every x/vt emulator this package runs: a Mux's
// (newMuxLimits). A LineMux builds none. A variable so a test can count
// constructions where they happen.
var newEmulator = vt.NewEmulator

// lineFrame is the largest live frame a LineMux queues to a session: a PTY
// read's size, so a session's queue holds at most queueCap of them, as it does
// for a Mux.
const lineFrame = 32 << 10

// droppedNotice is what a session too slow to keep up sees in place of the
// output it missed.
const droppedNotice = "\r\n[harness] output dropped: this client fell behind; the run's stream file has all of it\r\n"

// sessionHost is what a Session calls back into: a Mux, or a LineMux.
type sessionHost interface {
	Input(s *Session, p []byte)
	Resize(s *Session, cols, rows int)
	Detach(s *Session)
	// renderSnapshotLocked is what replaces a coalesced backlog. The host's
	// lock is held.
	renderSnapshotLocked() []byte
}

// Attacher is a mux an attach session opens on: a Mux for a harness with a
// terminal, a LineMux for one without.
type Attacher interface {
	Attach(id uint32, mode protocol.AttachMode, cols, rows int, write func([]byte) error) *Session
}

// LineMux is the data plane of a harness whose runs have no terminal: a
// byte-bounded ring of its recent output lines and its live attach sessions.
// It implements io.Writer for the supervisor's pipe readers. Every field is
// guarded by mu, and Write never blocks on a client.
type LineMux struct {
	name string

	mu       sync.Mutex
	ring     *ring
	sessions map[*Session]struct{}
}

// newLineMux builds a LineMux whose recent-lines ring is bounded by lim.
func newLineMux(name string, lim RingLimits) *LineMux {
	return &LineMux{name: name, ring: newRing(lim), sessions: make(map[*Session]struct{})}
}

// Name returns the harness name this mux serves.
func (m *LineMux) Name() string { return m.name }

// Write appends output to the ring and queues it to every session. The
// supervisor writes whole CRLF-terminated lines, but any bytes are accepted.
func (m *LineMux) Write(p []byte) (int, error) {
	n := len(p)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ring.Write(p)
	if len(m.sessions) == 0 {
		return n, nil
	}
	for len(p) > 0 {
		k := min(len(p), lineFrame)
		// The caller may reuse p; one private copy per frame is shared
		// read-only by every session.
		frame := append([]byte(nil), p[:k]...)
		for s := range m.sessions {
			s.enqueueLocked(frame)
		}
		p = p[k:]
	}
	return n, nil
}

// Attach opens a session: the recent lines first, then the live lines, with
// nothing able to arrive in between (both are queued under mu).
func (m *LineMux) Attach(id uint32, mode protocol.AttachMode, cols, rows int, write func([]byte) error) *Session {
	s := &Session{
		id:        id,
		mode:      mode,
		mux:       m,
		cols:      cols,
		rows:      rows,
		write:     write,
		out:       make(chan []byte, queueCap),
		closed:    make(chan struct{}),
		createdAt: time.Now(),
	}
	m.mu.Lock()
	for _, f := range m.ring.tailFrames() {
		s.enqueueLocked(f)
	}
	m.sessions[s] = struct{}{}
	m.mu.Unlock()
	s.wg.Add(1)
	go s.pump()
	return s
}

// Resize records a session's viewport, for describe. There is no terminal to
// size.
func (m *LineMux) Resize(s *Session, cols, rows int) {
	m.mu.Lock()
	s.cols, s.rows = cols, rows
	m.mu.Unlock()
}

// Detach tears down one session. Safe to call more than once.
func (m *LineMux) Detach(s *Session) {
	m.mu.Lock()
	delete(m.sessions, s)
	m.mu.Unlock()
	s.stop()
}

// Input drops keystrokes: a pipe run's stdin is /dev/null.
func (m *LineMux) Input(*Session, []byte) {}

// renderSnapshotLocked is the notice that replaces a coalesced backlog.
func (m *LineMux) renderSnapshotLocked() []byte { return []byte(droppedNotice) }

// SessionCount reports the number of live sessions.
func (m *LineMux) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Snapshot returns the live sessions for describe. There is no viewport, so
// Cols and Rows are zero and no session sets a minimum.
func (m *LineMux) Snapshot() MuxSnapshot {
	m.mu.Lock()
	sessions := make([]SessionInfo, 0, len(m.sessions))
	for s := range m.sessions {
		sessions = append(sessions, SessionInfo{ID: s.id, Mode: s.mode, Cols: s.cols, Rows: s.rows, CreatedAt: s.createdAt})
	}
	m.mu.Unlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	return MuxSnapshot{Sessions: sessions}
}

// Output is one harness's output sink, handed to its supervisor through
// ManagerOptions.ExtraOutFor. Raw PTY bytes arrive through Write and go to
// the harness's Mux; a pipe run takes LineOut and writes its LineMux. Each
// records which mode the harness's latest run used, and builds its mux on
// first use.
type Output struct {
	r    *Registry
	name string
}

// Write tees a PTY run's raw bytes into the harness's Mux.
func (o *Output) Write(p []byte) (int, error) {
	return o.r.muxFor(o.name).Write(p)
}

// LineOut is where a pipe run's output lines go: the harness's LineMux. The
// supervisor asks for it once per pipe run.
func (o *Output) LineOut() io.Writer { return o.r.lineMuxFor(o.name) }

// Prime is called as the harness's supervisor is built. A harness with a
// terminal gets its Mux now, as every harness used to, so describe and the
// logs reply report its born viewport before anything attaches. A structured
// one-shot gets nothing: its LineMux comes with its first run or attach, and
// it never gets a Mux or an emulator.
func (o *Output) Prime(pipes bool) {
	if !pipes {
		o.r.Mux(o.name)
	}
}
