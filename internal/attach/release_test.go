package attach

// Governing: SPEC-0002 REQ "Emulator Memory" (every x/vt emulator the daemon
// creates keeps no unread scrollback and has its reply pump released when its
// owner is done) and REQ "Attach Session" (a session still attached to a
// removed Mux keeps working); ADR-0003; stump.wtf/harness#142 (why release
// closes the input pipe rather than calling Emulator.Close). Reported as
// https://github.com/stump-wtf/harness/issues/18.

import (
	"bytes"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/testwait"
)

// eventually polls cond until it holds or Budget(t, want) runs out.
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

// TestRegistryRemoveReleasesMux: removing a harness's Mux must end its reply
// pump, and with it the last reference that kept the Mux — emulator, ring and
// all — reachable. Before the fix the pump parked in Read forever, so every
// project harness ever torn down stayed in the heap.
//
// Reachability is the property, so that is what this checks, through weak
// pointers: a pump still parked holds both the Mux (its receiver) and the
// emulator (inside Read), and neither can be collected. Counting pump
// goroutines instead would race every other test's Mux, whose pumps exit on
// their own schedule now that Remove releases them.
func TestRegistryRemoveReleasesMux(t *testing.T) {
	r := NewRegistry(100)
	m := r.Mux("reduit/agent")
	if _, err := m.Write([]byte("some output\r\n")); err != nil {
		t.Fatal(err)
	}
	muxGone := weak.Make(m)
	termGone := weak.Make(m.term.(*vt.Emulator))
	m = nil // drop the test's own reference: only the pump may hold it now

	r.Remove("reduit/agent")

	if !eventually(t, 2*time.Second, func() bool {
		runtime.GC()
		return muxGone.Value() == nil && termGone.Value() == nil
	}) {
		t.Fatalf("after Remove the Mux is reachable = %v, its emulator = %v: pumpReplies is still parked",
			muxGone.Value() != nil, termGone.Value() != nil)
	}
}

// TestRemovedMuxKeepsServingAttachedSession: Remove releases the reply pump
// while a client may still be attached. That session keeps receiving live
// output, a fresh snapshot still renders, and a guest query — whose reply now
// has nowhere to go — must not block Write, which is the PTY reader.
func TestRemovedMuxKeepsServingAttachedSession(t *testing.T) {
	r := NewRegistry(100)
	m := r.Mux("reduit/agent")
	var c collector
	s := m.Attach(1, protocol.AttachRW, 80, 24, c.write)
	defer m.Detach(s)

	r.Remove("reduit/agent")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Write([]byte("\x1b[c\x1b[?2026$pafter-remove\r\n"))
	}()
	select {
	case <-done:
	case <-time.After(testwait.Budget(t, 2*time.Second)):
		t.Fatal("Write blocked on a query after Remove released the reply pump")
	}
	waitForFrameContaining(t, &c, []byte("after-remove"))

	m.mu.Lock()
	snap := m.renderSnapshotLocked()
	m.mu.Unlock()
	if !bytes.Contains(snap, []byte("after-remove")) {
		t.Fatalf("snapshot after Remove lost the screen: %q", snap)
	}
}

// TestMuxKeepsNoScrollback: nothing reads the Mux emulator's scrollback —
// attach replays history from the raw-byte ring — so it must not keep x/vt's
// default 10,000 rows (~85 MiB at 80 columns) for every harness.
func TestMuxKeepsNoScrollback(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	defer m.releaseReplies()
	for i := 0; i < 60; i++ {
		_, _ = m.Write([]byte("a line long enough to occupy most of the eighty column screen width\r\n"))
	}
	m.mu.Lock()
	n := m.term.ScrollbackLen()
	m.mu.Unlock()
	if n > 1 {
		t.Fatalf("mux emulator holds %d scrollback rows after 60 lines; want at most 1", n)
	}
}
