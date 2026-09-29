package supervisor

// Governing: issue #835 — "enabled intent flips to false with no trace".
// Every change of the enabled intent must be recorded in
// Snapshot.LastIntent with its source (and the peer, for socket verbs),
// and the record must survive Restore across a daemon restart.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestIntentTracesVerbStartStopRestart(t *testing.T) {
	s := newTestSupervisor(t, shHarness("intent", "while true; do sleep 0.02; done", 0), fastPolicy())
	s.Start()
	waitState(t, s, core.StateRunning)

	if !s.Snapshot().Enabled {
		t.Fatal("start must set the enabled intent")
	}
	if got := s.Snapshot().LastIntent.Source; got != "verb:start" {
		t.Fatalf("last intent source = %q, want verb:start", got)
	}
	if s.Snapshot().LastIntent.At.IsZero() {
		t.Fatal("last intent must carry a timestamp")
	}

	s.Stop()
	waitState(t, s, core.StateStopped)
	if snap := s.Snapshot(); snap.Enabled {
		t.Fatal("stop must clear the enabled intent")
	} else if got := snap.LastIntent.Source; got != "verb:stop" {
		t.Fatalf("last intent source = %q, want verb:stop", got)
	}

	s.Restart()
	waitState(t, s, core.StateRunning)
	if got := s.Snapshot().LastIntent.Source; got != "verb:restart" {
		t.Fatalf("last intent source = %q, want verb:restart", got)
	}
}

func TestIntentRecordsSourceAndPeerFromSocketVerb(t *testing.T) {
	s := newTestSupervisor(t, shHarness("intent-peer", "while true; do sleep 0.02; done", 0), fastPolicy())
	s.StartWithPeer(TriggerManual, "uid=1 pid=2")
	waitState(t, s, core.StateRunning)

	snap := s.Snapshot()
	if got := snap.LastIntent.Source; got != "verb:start" {
		t.Fatalf("last intent source = %q, want verb:start", got)
	}
	if got := snap.LastIntent.Peer; got != "uid=1 pid=2" {
		t.Fatalf("last intent peer = %q, want uid=1 pid=2", got)
	}

	s.StopBy("guard", "uid=3 pid=4")
	waitState(t, s, core.StateStopped)
	snap = s.Snapshot()
	if got := snap.LastIntent.Source; got != "guard" {
		t.Fatalf("last intent source = %q, want guard", got)
	}
	if got := snap.LastIntent.Peer; got != "uid=3 pid=4" {
		t.Fatalf("last intent peer = %q, want uid=3 pid=4", got)
	}
}

// The #835 incident path: a clean exit under restart="no" finalizes the run
// and clears the intent with nothing logged — it must now record "policy".
func TestIntentPolicyFinalExitRecordsPolicy(t *testing.T) {
	s := newTestSupervisor(t, shHarnessWithRestart("intent-policy", "exit 0", 5*time.Millisecond, core.RestartNo), noFlapPolicy())
	s.Start()
	waitState(t, s, core.StateStopped)

	snap := s.Snapshot()
	if snap.Enabled {
		t.Fatal("restart=no policy must clear the enabled intent")
	}
	if got := snap.LastIntent.Source; got != "policy" {
		t.Fatalf("last intent source = %q, want policy", got)
	}
}

// A start on a harness whose intent is already enabled is not a change: it
// must not overwrite the recorded one (only real flips trace).
func TestIntentNoOpStartDoesNotOverwrite(t *testing.T) {
	s := newTestSupervisor(t, shHarness("intent-noop", "while true; do sleep 0.02; done", 0), fastPolicy())
	prev := time.Now().Add(-time.Hour)
	s.Restore(true, false, 0, 0, prev, prev, IntentChange{At: prev, Source: "verb:start", Peer: "uid=9 pid=9"})

	s.Start()
	waitState(t, s, core.StateRunning)

	snap := s.Snapshot()
	if got := snap.LastIntent.At; !got.Equal(prev) {
		t.Fatalf("last intent at = %v, want %v (a no-op start must not re-record)", got, prev)
	}
	if got := snap.LastIntent.Source; got != "verb:start" {
		t.Fatalf("last intent source = %q, want verb:start", got)
	}
	if got := snap.LastIntent.Peer; got != "uid=9 pid=9" {
		t.Fatalf("last intent peer = %q, want uid=9 pid=9", got)
	}
}

// The persisted record rides Restore into the snapshot so `harness describe`
// answers "who last flipped intent" across daemon restarts.
func TestRestoreCarriesLastIntent(t *testing.T) {
	s := newTestSupervisor(t, shHarness("intent-restore", "while true; do sleep 0.02; done", 0), fastPolicy())
	prev := time.Now().Add(-time.Hour)
	rec := IntentChange{At: prev, Source: "policy", Peer: ""}
	s.Restore(false, true, 0, 0, prev, prev, rec)

	snap := s.Snapshot()
	if snap.LastIntent != rec {
		t.Fatalf("restored last intent = %+v, want %+v", snap.LastIntent, rec)
	}
}

// ---- ADR-0007 / #835: the last intent change survives state.json ----------

func TestManagerPersistsAndRestoresLastIntent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")

	cfg := managerCfg(shHarness("pintent", "while true; do sleep 0.02; done", 0))
	m1 := NewManager(cfg, ManagerOptions{Policy: fastPolicy(), StatePath: statePath, LogDir: logDir})
	if err := m1.Restore(); err != nil {
		t.Fatal(err)
	}
	m1.Start("pintent")
	waitFor(t, 3*time.Second, "harness reaches running", func() bool {
		snap, _ := m1.Snapshot("pintent")
		return snap.State == core.StateRunning
	})
	m1.Stop("pintent")
	waitFor(t, 3*time.Second, "harness reaches stopped", func() bool {
		snap, _ := m1.Snapshot("pintent")
		return snap.State == core.StateStopped
	})
	if err := m1.Save(); err != nil {
		t.Fatal(err)
	}
	m1.Close()

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"last_intent_source": "verb:stop"`)) {
		t.Fatalf("state.json does not record the last intent source:\n%s", raw)
	}

	m2 := NewManager(cfg, ManagerOptions{Policy: fastPolicy(), StatePath: statePath, LogDir: logDir})
	t.Cleanup(m2.Close)
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	snap, ok := m2.Snapshot("pintent")
	if !ok {
		t.Fatal("harness missing after restore")
	}
	if snap.Enabled {
		t.Fatal("intent not restored: expected enabled=false (was stopped before shutdown)")
	}
	if got := snap.LastIntent.Source; got != "verb:stop" {
		t.Fatalf("restored last intent source = %q, want verb:stop", got)
	}
	if snap.LastIntent.At.IsZero() {
		t.Fatal("restored last intent must carry a timestamp")
	}
}
