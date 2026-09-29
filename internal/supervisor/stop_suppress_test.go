// Operator-Stop Suppression Tests
//
// `harness stop` on a triggered harness pauses its schedule: schedule and
// catch-up firings are recorded skipped until the next explicit start, across
// daemon restarts; event-source firings deliberately still go through. These
// tests drive a real Manager the same way runs_test.go does, so a skip is
// only ever produced by the path production uses.
//
// Governing: SPEC-0008 REQ "Firing And Overlap", REQ "Run Record Fields";
// stump.wtf/harness#786.
//
// @joestump-agent 09/28/2026 - Added for stump.wtf/harness#786.

package supervisor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// TestStopSuppressesScheduledFiring: after `harness stop`, a cron firing is
// recorded skipped with reason stopped and starts nothing.
func TestStopSuppressesScheduledFiring(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "sleep 30")), fastPolicy())

	if !m.Stop("sweep") {
		t.Fatal("Stop: unknown harness")
	}
	d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	if !ok {
		t.Fatal("StartRun: unknown harness")
	}
	if d.Kind != DecisionSkipped {
		t.Fatalf("firing after stop = %s, want skipped", d.Kind)
	}
	if d.Run.Reason != ReasonStopped {
		t.Errorf("reason = %s, want stopped", d.Run.Reason)
	}
	if snap, _ := m.Snapshot("sweep"); snap.OperatorStopped != true {
		t.Errorf("snapshot does not carry the operator stop")
	}
}

// TestStartReArmsSuppressedSchedule: an explicit start clears the suppression,
// and the next firing runs.
func TestStartReArmsSuppressedSchedule(t *testing.T) {
	e := newRunsEnv(t)
	m, close := e.manager(t, sweepCfg(sweep("sweep", "true")), fastPolicy())

	m.Stop("sweep")
	if d, _ := m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule}); d.Kind != DecisionSkipped {
		t.Fatalf("firing after stop = %s, want skipped", d.Kind)
	}
	m.Start("sweep")
	if snap, _ := m.Snapshot("sweep"); snap.OperatorStopped {
		t.Fatal("start did not clear the operator stop")
	}
	// The start's manual run must be out of the way first, or the firing
	// skips on overlap — a different guard with the same verdict.
	waitFor(t, 5*time.Second, "the manual run ended", func() bool {
		snap, _ := m.Snapshot("sweep")
		return snap.State == core.StateStopped
	})
	d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	if !ok || d.Kind != DecisionStarted {
		t.Fatalf("firing after start = %v, want started (ok=%v)", d, ok)
	}
	m.Stop("sweep")
	close()
}

// TestStopSuppressionSurvivesRestore: a schedule paused by `harness stop`
// stays paused when the daemon restarts, and an explicit start on the new
// daemon re-arms it.
func TestStopSuppressionSurvivesRestore(t *testing.T) {
	e := newRunsEnv(t)
	cfg := sweepCfg(sweep("sweep", "true"))
	m1, close1 := e.manager(t, cfg, fastPolicy())
	m1.Stop("sweep")
	close1()

	m2, _ := e.manager(t, cfg, fastPolicy())
	d, ok := m2.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	if !ok || d.Kind != DecisionSkipped || d.Run.Reason != ReasonStopped {
		t.Fatalf("firing after restore = %v (ok=%v), want skipped/stopped", d, ok)
	}
	m2.Start("sweep")
	waitFor(t, 5*time.Second, "the manual run ended", func() bool {
		snap, _ := m2.Snapshot("sweep")
		return snap.State == core.StateStopped
	})
	if d, _ := m2.StartRun("sweep", RunRequest{Trigger: TriggerSchedule}); d.Kind != DecisionStarted {
		t.Fatalf("firing after start = %v, want started", d.Kind)
	}
	m2.Stop("sweep")
}

// TestManualTriggerRunsAStoppedHarness: `harness trigger` is the operator
// asking for exactly one run, so it goes through despite the suppression —
// and leaves the schedule suppressed.
func TestManualTriggerRunsAStoppedHarness(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "sleep 30")), fastPolicy())

	m.Stop("sweep")
	d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerManual})
	if !ok || d.Kind != DecisionStarted {
		t.Fatalf("manual trigger after stop = %v (ok=%v), want started", d, ok)
	}
	rs := waitRuns(t, m, "sweep", "manual run in flight", outcomesAre(OutcomeRunning))
	if rs[0].Trigger != TriggerManual {
		t.Errorf("trigger = %s, want manual", rs[0].Trigger)
	}
	m.Stop("sweep")
}

// TestStopSuppressesEventFiringNot: a stop does NOT suppress event sources —
// it cancels the run in flight, and the next webhook firing starts a new one
// (the coalesce lifecycle the runs_coalesce tests pin). Only the cron is
// suppressed (stump.wtf/harness#786).
func TestStopDoesNotSuppressEventFiring(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "sleep 30")), fastPolicy())

	m.Stop("sweep")
	d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerWebhook})
	if !ok || d.Kind != DecisionStarted {
		t.Fatalf("webhook firing after stop = %v (ok=%v), want started", d, ok)
	}
	m.Stop("sweep")
}

// TestUnstoppedScheduleStillFires: the suppression only exists after an
// operator stop — the ordinary one-shot lifecycle (enabled false between
// runs, issue #159) still fires on its schedule.
func TestUnstoppedScheduleStillFires(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "sleep 30")), fastPolicy())

	d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	if !ok || d.Kind != DecisionStarted {
		t.Fatalf("first firing = %v (ok=%v), want started", d, ok)
	}
	m.Stop("sweep")
}

// TestStopSuppressesHoursOpenCatchUp: the hours-open catch-up goes through
// the same suppression as a cron firing — a stop lands, then the gate pass
// settles its outside_hours skips: the settle-up happens (the flag clears, no
// backlog accrues while disarmed) but the catch_up run it would ask for is
// recorded skipped with reason stopped, and nothing runs. Before the loop
// gate this was the one path that reached startRun past the Manager's
// snapshot check (stump.wtf/harness#786).
func TestStopSuppressesHoursOpenCatchUp(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	m, _ := e.manager(t, sweepCfg(gatedTrigger("sweep", marker, true)), fastPolicy())

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	for i := 0; i < 3; i++ {
		m.SkipRun("sweep", req, ReasonOutsideHours)
	}
	if !m.HoursSkipped("sweep") {
		t.Fatal("HoursSkipped = false before the stop; the test setup is wrong")
	}
	m.Stop("sweep")

	d, ok := m.OpenFirings("sweep")
	if !ok {
		t.Fatal("OpenFirings: unknown harness")
	}
	if d.Kind != DecisionSkipped || d.Run.Reason != ReasonStopped {
		t.Fatalf("hours-open catch-up after stop = %v (reason %s), want skipped/stopped", d.Kind, d.Run.Reason)
	}
	if m.HoursSkipped("sweep") {
		t.Error("HoursSkipped still set after the settle-up; the backlog accrues while disarmed")
	}
	if n := spawns(t, marker); n != 0 {
		t.Errorf("%d processes ran for the catch-up, want 0", n)
	}
	m.Stop("sweep")
}

// TestStopWhileRunInFlightSuppressesNextFiring: the stop lands while a
// scheduled run is mid-flight; once the graceful stop completes, the next
// firing is skipped with reason stopped rather than starting after the stop.
func TestStopWhileRunInFlightSuppressesNextFiring(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "sleep 30")), fastPolicy())

	d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	if !ok || d.Kind != DecisionStarted {
		t.Fatalf("scheduled firing = %v (ok=%v), want started", d, ok)
	}
	waitRuns(t, m, "sweep", "the scheduled run is in flight", outcomesAre(OutcomeRunning))
	if !m.Stop("sweep") {
		t.Fatal("Stop: unknown harness")
	}
	waitFor(t, 5*time.Second, "the stop completed", func() bool {
		snap, _ := m.Snapshot("sweep")
		return snap.State == core.StateStopped
	})
	if d, ok := m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule}); !ok ||
		d.Kind != DecisionSkipped || d.Run.Reason != ReasonStopped {
		t.Fatalf("next firing after stop = %v (ok=%v), want skipped/stopped", d, ok)
	}
	m.Stop("sweep")
}
