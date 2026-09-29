package attach

// Governing: SPEC-0002 REQ "Attach Session"; ADR-0003 (one x/vt emulator per
// harness); ADR-0007 (per-harness ring). The Registry owns one Mux per harness
// name and hands the supervisor Manager a WriterFor(name) tee target through the
// ManagerOptions.ExtraOutFor hook, closing the wiring the reviewer flagged as
// missing: raw PTY output now tees to BOTH the durable log and this vt ring.

import (
	"io"
	"sync"
	"syscall"
	"time"
)

// Controller is the slice of the supervisor Manager the attach layer needs to
// drive the real PTY: apply the smallest-attached-wins resize, deliver
// read-write keystrokes, and signal the guest's process group (the SIGWINCH
// re-assert, stump.wtf/harness#182). *supervisor.Manager satisfies it.
type Controller interface {
	Resize(name string, cols, rows int) bool
	WriteInput(name string, p []byte) bool
	SignalGroup(name string, sig syscall.Signal) bool
}

// Registry maps harness name → Mux, creating each lazily on first use. It is
// safe for concurrent use.
type Registry struct {
	limits RingLimits

	mu    sync.Mutex
	muxes map[string]*Mux
	ctrl  Controller
	// lines holds the LineMux of each harness whose runs have no terminal,
	// and pipes the mode of each harness's latest run: true for a pipe run,
	// false for a PTY run, absent before its first (lines.go).
	lines map[string]*LineMux
	pipes map[string]bool
}

// NewRegistry builds a registry whose muxes each keep ringLines of scrollback
// (DefaultRingLines when <=0) within the default byte budget.
func NewRegistry(ringLines int) *Registry {
	return NewRegistryLimits(RingLimits{Lines: ringLines})
}

// NewRegistryLimits builds a registry whose muxes' scrollback rings are each
// bounded by lim (zero fields take defaults; ADR-0007).
func NewRegistryLimits(lim RingLimits) *Registry {
	return &Registry{limits: lim, muxes: make(map[string]*Mux), lines: make(map[string]*LineMux), pipes: make(map[string]bool)}
}

// Limits reports the limits each new Mux's ring gets, defaults filled in.
func (r *Registry) Limits() RingLimits { return r.limits.withDefaults() }

// SetController wires the Manager the muxes call back into for PTY resize and
// input. Call it once, before the daemon starts harnesses or serves clients, so
// the callbacks are visible to the goroutines that later use them.
func (r *Registry) SetController(c Controller) {
	r.mu.Lock()
	r.ctrl = c
	r.mu.Unlock()
}

// Mux returns the Mux for name, creating it if needed. The same instance is
// returned for a name across process restarts of that harness, so attach state
// (screen + scrollback + sessions) survives a supervised process respawn while
// the daemon lives (ADR-0007).
func (r *Registry) Mux(name string) *Mux {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.muxLocked(name)
}

// muxLocked is Mux with r.mu held.
func (r *Registry) muxLocked(name string) *Mux {
	if m, ok := r.muxes[name]; ok {
		return m
	}
	m := newMuxLimits(name, r.limits,
		func(cols, rows int) {
			if c := r.controller(); c != nil {
				c.Resize(name, cols, rows)
			}
		},
		func(p []byte) {
			if c := r.controller(); c != nil {
				c.WriteInput(name, p)
			}
		},
		func() {
			if c := r.controller(); c != nil {
				c.SignalGroup(name, syscall.SIGWINCH)
			}
		},
	)
	r.muxes[name] = m
	return m
}

// WriterFor is the ManagerOptions.ExtraOutFor adapter: it returns the
// harness's Output, which tees a PTY run's raw bytes into its Mux and a pipe
// run's lines into its LineMux, building each on first use (lines.go).
func (r *Registry) WriterFor(name string) io.Writer { return &Output{r: r, name: name} }

// muxFor is the Mux a PTY run of name writes, recording that its latest run
// has a terminal.
func (r *Registry) muxFor(name string) *Mux {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pipes[name] = false
	return r.muxLocked(name)
}

// lineMuxFor is the LineMux a pipe run of name writes, recording that its
// latest run has no terminal.
func (r *Registry) lineMuxFor(name string) *LineMux {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pipes[name] = true
	return r.lineMuxLocked(name)
}

// lineMuxLocked returns name's LineMux, creating it. Caller holds r.mu.
func (r *Registry) lineMuxLocked(name string) *LineMux {
	l, ok := r.lines[name]
	if !ok {
		l = newLineMux(name, r.limits)
		r.lines[name] = l
	}
	return l
}

// AttachTarget is the mux an attach session for name opens on: the one its
// latest run writes or, before its first run, the one its definition says its
// runs will write (pipes: a structured one-shot, supervisor.RunsOnPipes). A
// session opened before a reload changed the harness's kind stays on the old
// mux until it reattaches.
func (r *Registry) AttachTarget(name string, pipes bool) Attacher {
	r.mu.Lock()
	defer r.mu.Unlock()
	if latest, ok := r.pipes[name]; ok {
		pipes = latest
	}
	if pipes {
		return r.lineMuxLocked(name)
	}
	return r.muxLocked(name)
}

// SizeFor is the ManagerOptions.SizeFor adapter: it reports the harness's
// authoritative (smallest-attached-wins) viewport so a freshly spawned PTY is
// born at the size of whoever is attached, rather than 80×24 (ADR-0003). A
// harness nobody has ever attached to reports the mux default, which is 80×24 —
// the historical behaviour.
func (r *Registry) SizeFor(name string) (int, int) { return r.Mux(name).Size() }

// SnapshotFor returns a point-in-time view of name's attach plane — the
// authoritative viewport and every live session — for `describe` visibility
// (#183). Unlike Mux/SizeFor it never creates a Mux: describing a harness
// nobody has ever attached to must not materialize emulator state for it.
//
// A harness whose latest run had no terminal reports its LineMux: its sessions
// and no viewport (lines.go).
func (r *Registry) SnapshotFor(name string) (MuxSnapshot, bool) {
	r.mu.Lock()
	m, hasMux := r.muxes[name]
	l, hasLines := r.lines[name]
	pipes, known := r.pipes[name]
	r.mu.Unlock()
	switch {
	case hasLines && (pipes || !known || !hasMux):
		return l.Snapshot(), true
	case hasMux:
		return m.Snapshot(), true
	}
	return MuxSnapshot{}, false
}

// ScreenFor returns the harness's visible screen and idle age, without
// creating a Mux: capturing (or judging) a harness nobody has ever teed output
// for must not materialize emulator state for it (the same discipline
// SnapshotFor applies for describe). ok is false when no Mux exists.
func (r *Registry) ScreenFor(name string, now time.Time) (ScreenState, bool) {
	r.mu.Lock()
	m, ok := r.muxes[name]
	r.mu.Unlock()
	if !ok {
		return ScreenState{}, false
	}
	return m.ScreenState(now), true
}

// AnsiFor returns the harness's visible screen as an ANSI repaint, the same
// non-creating discipline as ScreenFor (issue #735). ok is false when no Mux
// exists.
func (r *Registry) AnsiFor(name string) ([]byte, bool) {
	r.mu.Lock()
	m, ok := r.muxes[name]
	r.mu.Unlock()
	if !ok {
		return nil, false
	}
	return m.AnsiScreen(), true
}

// Remove drops the Mux registered for name, releasing its vt emulator and
// scrollback ring for collection. The supervisor Manager calls this (via
// ManagerOptions.DropExtraOut) when a project harness is deregistered, so an
// up→down→up cycle starts from a fresh screen instead of resurfacing the dead
// incarnation's scrollback — and churning project names cannot grow the map
// without bound (SPEC-0004 REQ "Tear Down"; ADR-0009). Sessions still attached
// keep their now-quiescent Mux until they detach; a later re-registration gets
// a brand-new one.
//
// The dropped Mux's reply pump is released (Mux.releaseReplies), so its
// pumpReplies goroutine exits instead of parking in Read forever and keeping
// the whole emulator reachable after the map has let go of it. It is released
// by closing the emulator's input pipe, never with Emulator.Close: x/vt guards
// Read/Close with a plain bool (no mutex, no atomic), so Close races Read on
// pumpReplies' own goroutine under -race (stump.wtf/harness#142). A session
// still attached keeps working: snapshots and the live fan-out never touch
// the pipe, and a reply the emulator synthesizes afterwards fails against the
// closed pipe rather than blocking Write. Governing: SPEC-0002 REQ "Emulator
// Memory".
//
// The harness's LineMux and recorded mode go with it (lines.go). A LineMux has
// no emulator and no goroutine, so dropping it is all its release needs.
func (r *Registry) Remove(name string) {
	r.mu.Lock()
	m := r.muxes[name]
	delete(r.muxes, name)
	delete(r.lines, name)
	delete(r.pipes, name)
	r.mu.Unlock()
	if m != nil {
		m.releaseReplies()
	}
}

// controller reads the wired controller under the lock.
func (r *Registry) controller() Controller {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctrl
}
