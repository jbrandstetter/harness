package notify

// Watcher Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725 — which
// lifecycle events become which notifications, what each message says, and
// when `recovered` may fire. The daemon's own wiring of these sources is
// covered in cmd/harness (daemon_notify_test.go) against a real Manager.
//
// A test that expects silence settles the rig first: the watcher has handled
// every event sent and every delivery they queued has finished, so the count
// is exact rather than whatever arrived within a sleep.
//
// @joestump-agent 09/28/2026 - Settle instead of polling files and sleeping
// 100ms before counting.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/loopguard"
	"github.com/stump-wtf/harness/internal/supervisor"
)

type fakeSource struct {
	ch   chan supervisor.Event
	snap supervisor.Snapshot
	dir  string
}

func (f *fakeSource) Events() (<-chan supervisor.Event, func()) {
	return f.ch, func() {
		defer func() { _ = recover() }()
		close(f.ch)
	}
}
func (f *fakeSource) Snapshot(string) (supervisor.Snapshot, bool) { return f.snap, true }
func (f *fakeSource) LogDir() string                              { return f.dir }
func (f *fakeSource) RunLogPath(name string, id int) string {
	return filepath.Join(f.dir, name, "run.log")
}

type watcherRig struct {
	src *fakeSource
	w   *Watcher
	d   *testDispatcher
	out string // where the recorder's deliveries land
}

func newWatcherRig(t *testing.T, events []string) *watcherRig {
	t.Helper()
	argv, out := NewRecorder(t)
	cfg := testConfig(t, argv)
	if events != nil {
		cfg.Events = events
	}
	d := newTestDispatcher(t, cfg, Options{})
	src := &fakeSource{ch: make(chan supervisor.Event, 16), dir: t.TempDir()}
	w := Watch(src, d.Dispatcher)
	t.Cleanup(w.Close)
	return &watcherRig{src: src, w: w, d: d, out: out}
}

// settle waits for the watcher to handle every event sent so far (closing it
// drains the fake's buffered channel) and for every delivery that queued,
// then returns what the hook received, which must be exactly want.
func (r *watcherRig) settle(t *testing.T, want int) []Received {
	t.Helper()
	r.w.Close()
	return r.d.settle(t, r.out, want)
}

func TestWatcherRunFailedQuotesTheRunLog(t *testing.T) {
	r := newWatcherRig(t, core.NotifyEvents)
	runLog := filepath.Join(r.src.dir, "nightly", "run.log")
	if err := os.MkdirAll(filepath.Dir(runLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLog, []byte("fetching\nfatal: repository not found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := 128
	// A successful run is not news.
	r.src.ch <- supervisor.Event{Kind: supervisor.EventRunFinished, Name: "nightly", Run: supervisor.RunRecord{RunID: 41, Outcome: supervisor.OutcomeSuccess}}
	r.src.ch <- supervisor.Event{Kind: supervisor.EventRunFinished, Name: "nightly", Run: supervisor.RunRecord{RunID: 42, Outcome: supervisor.OutcomeFailed, ExitCode: &code}}
	p := r.settle(t, 1)[0].Payload // one: the successful run must not notify
	if p.Event != core.NotifyRunFailed || p.RunID != 42 || p.Cause != "fatal: repository not found" ||
		p.Hint != "harness logs nightly --run 42" || !strings.Contains(p.Message, "run #42 failed (exit 128)") {
		t.Fatalf("payload = %+v", p)
	}
}

func TestWatcherFlapping(t *testing.T) {
	r := newWatcherRig(t, nil)
	r.src.snap = supervisor.Snapshot{State: core.StateRestarting, LastExitCode: 1}
	if err := os.WriteFile(filepath.Join(r.src.dir, "rc.log"), []byte("Error: You must be logged in to use Remote Control.\n2026/09/25 20:15:11 INFO exited code=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "rc", Restarts: 3, NextRetryIn: 8 * time.Second}
	p := r.settle(t, 1)[0].Payload
	if p.Event != core.NotifyFlapping || p.Restarts != 3 || p.State != string(core.StateRestarting) ||
		!strings.Contains(p.Message, `rc is crash-looping: 3 restarts, next retry in 8s (last exit 1): "Error: You must be logged in to use Remote Control."`) {
		t.Fatalf("payload = %+v", p)
	}
}

// `recovered` closes an alert the operator received — so only after one.
func TestWatcherRecoveredOnlyAfterAnAlert(t *testing.T) {
	r := newWatcherRig(t, nil)
	// Running with nothing open: an ordinary start, not a recovery. The
	// flapping sentinel behind it proves the start was consumed before the
	// loop stop below opens an alert.
	r.src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", From: core.StateStarting, To: core.StateRunning}
	r.src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "sentinel", Restarts: 2}
	r.d.waitDelivered(t, 1)
	r.w.LoopStopped(loopguard.Trip{Harness: "a", Tool: "mcp_gitea_issue_write", Count: 8})
	r.src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", From: core.StateStarting, To: core.StateRunning}
	// Exactly three: sentinel, loop_stopped, recovered.
	got := r.settle(t, 3)
	var loop, rec Payload
	for _, g := range got {
		switch g.Payload.Event {
		case core.NotifyLoopStopped:
			loop = g.Payload
		case core.NotifyRecovered:
			rec = g.Payload
		}
	}
	if loop.Tool != "mcp_gitea_issue_write" || loop.Count != 8 || loop.State != string(core.StateStopped) ||
		!strings.Contains(loop.Message, "stays down until `harness start a`") {
		t.Fatalf("loop_stopped payload = %+v", loop)
	}
	if rec.Harness != "a" || rec.Message != "a is running again (was loop-stopped)" {
		t.Fatalf("recovered payload = %+v", rec)
	}
}

// An alert the operator filtered out opens nothing: they must not hear of a
// recovery from something they were never told about.
func TestWatcherNoRecoveryForAnUnwantedEvent(t *testing.T) {
	r := newWatcherRig(t, []string{core.NotifyRecovered, core.NotifyFlapping})
	r.w.LoopStopped(loopguard.Trip{Harness: "a", Tool: "t", Count: 8})
	r.src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", To: core.StateRunning}
	// A wanted event too, so the missing recovery is silence from the
	// watcher, not a hook that never runs in this rig.
	r.src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "b", Restarts: 2}
	if got := r.settle(t, 1); got[0].Payload.Event != core.NotifyFlapping {
		t.Fatalf("deliveries = %+v, want the flapping one only", got)
	}
}

func TestWatcherSessionRotated(t *testing.T) {
	r := newWatcherRig(t, nil)
	r.w.SessionRotated(supervisor.SessionRotation{Harness: "crush-qwen", Turns: 5, Errors: 5, Archive: "/w/.crush/crush.db.wedged-20260926", Rotations: 1})
	r.w.SessionRotated(supervisor.SessionRotation{Harness: "crush-qwen-2", Turns: 4, Errors: 4, Failed: "could not restart the harness after archiving its store"})
	by := map[string]Payload{}
	for _, g := range r.settle(t, 2) {
		by[g.Payload.Harness] = g.Payload
	}
	ok := by["crush-qwen"]
	if ok.Event != core.NotifySessionRotated || ok.State != string(core.StateRunning) ||
		!strings.Contains(ok.Message, "5 of 5 recent turns failed on context-limit errors") ||
		!strings.Contains(ok.Message, "crush.db.wedged-20260926") {
		t.Fatalf("rotation payload = %+v", ok)
	}
	bad := by["crush-qwen-2"]
	if bad.State != string(core.StateStopped) || !strings.Contains(bad.Message, "rotation failed") ||
		!strings.Contains(bad.Message, "harness start crush-qwen-2") {
		t.Fatalf("failed rotation payload = %+v", bad)
	}
}
