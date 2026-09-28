package supervisor

// Governing: SPEC-0002 REQ "Emulator Memory" (every x/vt emulator the daemon
// creates keeps no unread scrollback and has its reply pump released when its
// owner is done); ADR-0003 "Memory cost of an x/vt emulator"; ADR-0007
// (amended: the durable log is sanitized through an emulator per spawn).
// Reported as https://github.com/stump-wtf/harness/issues/18.

import (
	"bytes"
	"runtime"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/harness/internal/testwait"
)

// drainPumpFrame is the entry frame of a sanitizer emulator's reply pump in a
// goroutine dump.
const drainPumpFrame = "supervisor.(*ptyHistory).drainReplies("

// goroutinesIn counts the live goroutines whose stack contains frame. It
// measures the leak itself — a parked reply pump — rather than the total
// goroutine count, which every other package-level goroutine moves too.
func goroutinesIn(frame string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return bytes.Count(buf[:n], []byte(frame))
		}
		buf = make([]byte, 2*len(buf))
	}
}

// liveHeap is the heap still reachable after two full collections: the first
// runs any finalizers, the second collects what they released.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// eventually polls cond until it holds or Budget(t, want) runs out, and
// reports which. Unlike waitFor it does not stop the test, so a failing run
// still measures, and reports, every property below.
func eventually(t *testing.T, want time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(testwait.Budget(t, want))
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// TestRespawnsReleaseTheirSanitizer drives the real path a resident harness
// takes on every restart — spawn, print, exit, respawn, and at the end
// Shutdown's closeLog — and checks the property the bug report is about: the
// daemon retains nothing per spawn.
//
// Each spawn builds a fresh x/vt emulator to sanitize its output (sanitize.go).
// Before the fix, that emulator's reply pump parked in Read forever, and the
// parked goroutine kept the whole emulator reachable: its 4 MiB parser buffer
// and every row the run scrolled, 112 bytes a cell. So the three checks are:
//
//   - reply pumps: every spawn's pump exits once its stream has ended, before
//     Shutdown runs, so a harness that restarts all day holds none of them;
//   - goroutines: after Shutdown the process is back to its baseline;
//   - heap: what is still reachable after Shutdown is less than two
//     emulators' parser buffers, however many spawns there were.
//
// Against the unfixed code all three fail. Measured there: 12 pumps still
// parked, 12 goroutines over the baseline after Shutdown, and 96 MiB of live
// heap left behind (2 MiB → 98 MiB), against 0, 0 and +37 KiB with the fix.
func TestRespawnsReleaseTheirSanitizer(t *testing.T) {
	const spawns = 12
	// 400 lines of ~80 columns: enough to scroll an 80×24 screen hundreds of
	// times, which is what filled the emulator's scrollback before the fix.
	script := `i=0; while [ $i -lt 400 ]; do echo "line $i $(printf '%070d' 0)"; i=$((i+1)); done`
	// Never flap or give up: this exercises plain restart-on-clean-exit, the
	// path every resident harness takes (TestCleanExitWhileEnabledRestarts).
	p := Policy{CrashWindow: time.Millisecond, CrashThreshold: 1000, MaxRestarts: 0, StopGrace: 500 * time.Millisecond}

	pumps0 := goroutinesIn(drainPumpFrame)
	g0 := runtime.NumGoroutine()
	h0 := liveHeap()

	s := New(shHarness("churn", script, 5*time.Millisecond), Options{Policy: p, Bus: NewBus(), LogCfg: LogConfig{Dir: t.TempDir()}})
	t.Cleanup(s.Shutdown)
	s.Start()
	waitFor(t, 20*time.Second, "the harness respawns repeatedly", func() bool {
		return s.Snapshot().RestartCount >= spawns
	})
	s.Stop()
	ran := s.Snapshot().RestartCount + 1

	// Per spawn, not only at Shutdown: closeLog runs once per supervisor
	// lifetime, so a release that only happened there would still leak one
	// emulator for every restart before it.
	pumpsOK := eventually(t, 5*time.Second, func() bool { return goroutinesIn(drainPumpFrame) <= pumps0 })
	pumps1 := goroutinesIn(drainPumpFrame)

	s.Shutdown()
	// A little slack for runtime goroutines unrelated to the supervisor; the
	// leak this guards is one goroutine per spawn, a dozen here.
	const slack = 3
	goroutinesOK := eventually(t, 5*time.Second, func() bool { return runtime.NumGoroutine() <= g0+slack })
	g1 := runtime.NumGoroutine()

	// One leaked emulator is at least its 4 MiB parser buffer (charmbracelet/x
	// #973), so a dozen leaked spawns are 48 MiB before a single scrolled row
	// is counted. Allow two emulators' worth of noise.
	const bound = 8 << 20
	h1 := liveHeap()
	var grew uint64
	if h1 > h0 {
		grew = h1 - h0
	}

	t.Logf("spawns=%d  reply pumps %d→%d  goroutines %d→%d  live heap %d KiB→%d KiB (+%d KiB)",
		ran, pumps0, pumps1, g0, g1, h0>>10, h1>>10, grew>>10)
	if !pumpsOK {
		t.Errorf("%d sanitizer reply pumps still parked after %d spawns ended; want 0", pumps1-pumps0, ran)
	}
	if !goroutinesOK {
		t.Errorf("goroutines %d after Shutdown, baseline %d; want within %d", g1, g0, slack)
	}
	if grew > bound {
		t.Errorf("live heap grew %d MiB across %d spawns; want < %d MiB: sanitizer emulators are still reachable",
			grew>>20, ran, bound>>20)
	}
}

// TestReleaseEndsTheReplyPump: release is what lets a sanitizer's emulator be
// collected, so after it the emulator's pump goroutine must be gone — and a
// second release, which closeLog makes after readOutput's, must be harmless.
func TestReleaseEndsTheReplyPump(t *testing.T) {
	before := goroutinesIn(drainPumpFrame)
	h := newPtyHistory(&bytes.Buffer{}, 80, 24)
	if got := goroutinesIn(drainPumpFrame); got != before+1 {
		t.Fatalf("reply pumps = %d after newPtyHistory, want %d", got, before+1)
	}
	h.Flush()
	h.release()
	h.release()
	if !eventually(t, 2*time.Second, func() bool { return goroutinesIn(drainPumpFrame) <= before }) {
		t.Fatalf("reply pumps = %d after release, want %d", goroutinesIn(drainPumpFrame), before)
	}
}

// TestReleasedSanitizerDoesNotBlockOnQueries: a reader that outlived
// closeLog's drain bound can still be writing when the emulator is released.
// A guest query then makes the emulator write a reply that no pump will ever
// read; the write must fail at once rather than block the PTY reader forever
// (the freeze stump.wtf/harness#299 describes). A release that merely stopped
// the pump without closing the pipe would hang here.
func TestReleasedSanitizerDoesNotBlockOnQueries(t *testing.T) {
	h := newPtyHistory(&bytes.Buffer{}, 80, 24)
	h.Flush()
	h.release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// DA1, DECRQM ?2026 and a cursor position report: each makes x/vt
		// write a reply.
		_, _ = h.Write([]byte("\x1b[c\x1b[?2026$p\x1b[6nstill here\r\n"))
	}()
	select {
	case <-done:
	case <-time.After(testwait.Budget(t, 2*time.Second)):
		t.Fatal("a query written after release blocked the writer")
	}
}

// panickyTerm is an emulator whose every Write panics, so the reset-first
// recovery in feedLocked fails too and the sanitizer falls back to a rebuild.
type panickyTerm struct{ *vt.Emulator }

func (panickyTerm) Write([]byte) (int, error) { panic("index out of range [24] with length 24") }

// TestPtyHistoryRebuildReleasesTheOldEmulator: when a panic cannot be healed
// by resetting the scroll region, the sanitizer replaces its emulator. The old
// one's reply pump must end with it rather than park forever beside its
// replacement, and the replacement must be capped and pumped like the first.
func TestPtyHistoryRebuildReleasesTheOldEmulator(t *testing.T) {
	before := goroutinesIn(drainPumpFrame)
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	defer h.release()
	h.mu.Lock()
	h.term = panickyTerm{h.term.(*vt.Emulator)}
	h.mu.Unlock()

	_, _ = h.Write([]byte("boom\r\n"))

	if !bytes.Contains(out.Bytes(), []byte("emulator reset")) {
		t.Fatalf("expected the rebuild marker in the log, got:\n%s", out.String())
	}
	if !eventually(t, 2*time.Second, func() bool { return goroutinesIn(drainPumpFrame) == before+1 }) {
		t.Fatalf("reply pumps = %d after a rebuild, want %d: the replaced emulator's pump is still parked",
			goroutinesIn(drainPumpFrame), before+1)
	}
	h.mu.Lock()
	_, rebuilt := h.term.(*vt.Emulator)
	h.mu.Unlock()
	if !rebuilt {
		t.Fatal("the sanitizer kept the panicking emulator instead of rebuilding")
	}
	// The replacement answers queries through its own pump, so this must not
	// block, and it must be capped like the emulator it replaced.
	for i := 0; i < 60; i++ {
		_, _ = h.Write([]byte("\x1b[ca line long enough to occupy most of the eighty column screen width\r\n"))
	}
	h.mu.Lock()
	n := h.term.ScrollbackLen()
	h.mu.Unlock()
	if n > 1 {
		t.Fatalf("rebuilt emulator holds %d scrollback rows; want at most 1", n)
	}
}

// TestPtyHistoryKeepsNoScrollback: the sanitizer diffs screens and never
// reads its emulator's scrollback, which x/vt otherwise fills to 10,000 rows
// of 112-byte cells (~85 MiB at 80 columns) for as long as the run lasts.
func TestPtyHistoryKeepsNoScrollback(t *testing.T) {
	h := newPtyHistory(&bytes.Buffer{}, 80, 24)
	defer h.release()
	for i := 0; i < 60; i++ {
		_, _ = h.Write([]byte("a line long enough to occupy most of the eighty column screen width\r\n"))
	}
	h.mu.Lock()
	n := h.term.ScrollbackLen()
	h.mu.Unlock()
	if n > 1 {
		t.Fatalf("sanitizer emulator holds %d scrollback rows after 60 lines; want at most 1", n)
	}
}
