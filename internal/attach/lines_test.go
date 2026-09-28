package attach

// Line mode tests (lines.go). Governing: ADR-0033 "Structured one-shots run on
// pipes"; SPEC-0002 REQ "Attach Session", REQ "Backpressure Isolation";
// SPEC-0017 REQ-18.

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/harness/internal/protocol"
)

// countEmulators counts every emulator this package builds until the test
// ends.
func countEmulators(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	orig := newEmulator
	newEmulator = func(w, h int) *vt.Emulator {
		n.Add(1)
		return orig(w, h)
	}
	t.Cleanup(func() { newEmulator = orig })
	return &n
}

// recorder is a session's client: what it was sent, in order.
type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recorder) write(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf.Write(p)
	return nil
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// waitForCond polls cond with a deadline.
func waitForCond(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A pipe run's lines reach an attach as recent lines, then live ones, in
// order, and nothing in the registry builds an emulator for them.
func TestLineMuxReplaysThenStreamsWithoutAnEmulator(t *testing.T) {
	built := countEmulators(t)
	r := NewRegistry(0)
	out := r.WriterFor("oneshot").(*Output)
	out.Prime(true)
	lines := out.LineOut()
	_, _ = lines.Write([]byte("{\"type\":\"system\"}\r\n"))
	_, _ = lines.Write([]byte("stderr says hi\r\n"))

	var rec recorder
	target := r.AttachTarget("oneshot", false) // the run's mode wins over the hint
	if _, ok := target.(*LineMux); !ok {
		t.Fatalf("a harness whose run used LineOut attached to %T", target)
	}
	s := target.Attach(1, protocol.AttachRO, 120, 40, rec.write)
	defer s.Detach()
	_, _ = lines.Write([]byte("{\"type\":\"result\"}\r\n"))

	want := "{\"type\":\"system\"}\r\nstderr says hi\r\n{\"type\":\"result\"}\r\n"
	waitForCond(t, "replay then live", func() bool { return rec.String() == want })
	if n := built.Load(); n != 0 {
		t.Fatalf("line mode built %d emulators, want 0", n)
	}
}

// Priming decides what exists before any run: a terminal harness gets its Mux
// (and emulator) up front, as it always did, a structured one-shot nothing.
// The first half shows the counter sees an emulator when one is built.
func TestPrimeBuildsAMuxOnlyForATerminalHarness(t *testing.T) {
	built := countEmulators(t)
	r := NewRegistry(0)
	r.WriterFor("resident").(*Output).Prime(false)
	if built.Load() != 1 {
		t.Fatalf("priming a terminal harness built %d emulators, want 1", built.Load())
	}
	if _, ok := r.SnapshotFor("resident"); !ok {
		t.Error("a primed terminal harness has no attach plane to describe")
	}
	r.WriterFor("oneshot").(*Output).Prime(true)
	if built.Load() != 1 {
		t.Fatalf("priming a structured one-shot built an emulator (%d total)", built.Load())
	}
	if _, ok := r.SnapshotFor("oneshot"); ok {
		t.Error("priming a structured one-shot materialized an attach plane")
	}
	// Before its first run, an attach goes by the definition.
	if _, ok := r.AttachTarget("oneshot", true).(*LineMux); !ok {
		t.Error("a never-run structured one-shot did not attach to a line mux")
	}
	if built.Load() != 1 {
		t.Fatalf("attaching to a never-run structured one-shot built an emulator (%d total)", built.Load())
	}
}

// A PTY write after a pipe run moves the harness back to its Mux (a reload
// changed its kind), and describe follows the latest run.
func TestAttachTargetFollowsTheLatestRun(t *testing.T) {
	r := NewRegistry(0)
	out := r.WriterFor("h").(*Output)
	_, _ = out.LineOut().Write([]byte("line\r\n"))
	if _, ok := r.AttachTarget("h", false).(*LineMux); !ok {
		t.Fatal("after a pipe run, attach did not go to the line mux")
	}
	_, _ = out.Write([]byte("pty bytes"))
	if _, ok := r.AttachTarget("h", true).(*Mux); !ok {
		t.Fatal("after a PTY run, attach did not go to the terminal mux")
	}
	if snap, _ := r.SnapshotFor("h"); snap.Cols == 0 {
		t.Error("describe after a PTY run reports the line mux's missing viewport")
	}
	r.Remove("h")
	if _, ok := r.SnapshotFor("h"); ok {
		t.Error("Remove left an attach plane behind")
	}
}

// A multi-megabyte line is queued in bounded frames, and a client too slow to
// take them has its backlog replaced by the dropped-output notice rather than
// a screen repaint.
func TestLineMuxBoundsFramesAndCoalescesToANotice(t *testing.T) {
	m := newLineMux("h", RingLimits{})
	block := make(chan struct{})
	var mu sync.Mutex
	var frames [][]byte
	s := m.Attach(1, protocol.AttachRO, 80, 24, func(p []byte) error {
		<-block
		mu.Lock()
		frames = append(frames, p)
		mu.Unlock()
		return nil
	})
	big := bytes.Repeat([]byte("x"), 10<<20) // 320 frames: past the queue
	_, _ = m.Write(append(big, '\r', '\n'))
	close(block)
	waitForCond(t, "the notice", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range frames {
			if string(f) == droppedNotice {
				return true
			}
		}
		return false
	})
	s.Detach()
	mu.Lock()
	defer mu.Unlock()
	for _, f := range frames {
		if len(f) > lineFrame {
			t.Fatalf("a frame of %d bytes was queued, want at most %d", len(f), lineFrame)
		}
	}
}

// Input goes nowhere (stdin is /dev/null), resize changes nothing but what
// describe shows, and describe reports sessions with no viewport.
func TestLineMuxHasNoTerminalControls(t *testing.T) {
	r := NewRegistry(0)
	ctrl := &countingController{}
	r.SetController(ctrl)
	_, _ = r.WriterFor("h").(*Output).LineOut().Write([]byte("a\r\n"))
	var rec recorder
	s := r.AttachTarget("h", true).Attach(7, protocol.AttachRW, 100, 30, rec.write)
	defer s.Detach()
	s.Input([]byte("ls\r"))
	s.Resize(50, 10)
	if ctrl.calls.Load() != 0 {
		t.Errorf("a line-mode session reached the controller %d times", ctrl.calls.Load())
	}
	snap, ok := r.SnapshotFor("h")
	if !ok || snap.Cols != 0 || snap.Rows != 0 || len(snap.Sessions) != 1 || snap.Sessions[0].Cols != 50 || snap.Sessions[0].SetsMin {
		t.Errorf("snapshot = %+v, want one 50x10 session and no viewport", snap)
	}
	waitForCond(t, "the replay", func() bool { return strings.Contains(rec.String(), "a\r\n") })
}

// countingController counts every call the attach layer makes into the
// supervisor.
type countingController struct{ calls atomic.Int64 }

func (c *countingController) Resize(string, int, int) bool {
	c.calls.Add(1)
	return true
}

func (c *countingController) WriteInput(string, []byte) bool {
	c.calls.Add(1)
	return true
}

func (c *countingController) SignalGroup(string, syscall.Signal) bool {
	c.calls.Add(1)
	return true
}
