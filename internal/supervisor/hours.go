package supervisor

// Operating Hours Close
//
// A gated harness (one with operating_hours) is held down outside its windows
// and started again when one opens, without its `enabled` intent ever
// changing. The scheduler's gate pass decides WHEN (internal/scheduler,
// gate.go); the hold and the release themselves are the generic hold-reason
// machinery in holds.go, of which hours is one reason. This file is what
// stays hours-shaped: the graceful close and the hours-reading helpers.
//
// A graceful close (hours_shutdown = "graceful", the default) marks the
// supervisor Closing and returns; the scheduler tick then drives
// Manager.CloseStep, which samples the daemon's turn-state watch
// (internal/runtrace) and sends the observation here. The close stops the
// harness at the first of: turn ended and settled, quiet long enough with no
// markers to settle, or the deadline — closeAt (the instant the harness went
// out of hours, not when the daemon noticed) plus the configured timeout,
// re-read from the staged config on every step so a reload applies to a close
// in progress. An immediate close, and any close of a harness that is not
// plain running, stops at once.
//
// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Gate Enforcement",
// REQ "Graceful Shutdown", REQ "Shutdown Mode", REQ "Operating Hours Reload";
// design.md § "Hold and Release on the Manager", § "Turn state from a
// daemon-side watcher"; SPEC-0003 REQ "Graceful Stop", REQ "Restart On Exit".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.
//
// @joestump-agent 09/21/2026 - Graceful close for #384: Closing marks a close
// in flight, closeStep advances it on the tick, and an exit while held is
// consumed by the gate instead of the restart policy.
//
// @joestump 10/04/2026 - Hold and release moved to holds.go, generalized from
// a held flag to a set of reasons (stump.wtf/harness#468).

import (
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// Close-timing constants (SPEC-0012 REQ "Graceful Shutdown"): how long a
// settled turn end is given for the agent to really stop talking, and how long
// a marker-less agent must stay silent before the close reads it as done.
const (
	closeSettle = 10 * time.Second
	closeQuiet  = 2 * time.Minute
)

// CloseStep advances a graceful close by one observation: the manager samples
// the turn-state watch and hands the answer to the loop, which stops the
// harness when the close is done waiting and otherwise leaves it running. A
// no-op when no close is in flight.
func (s *Supervisor) CloseStep(now time.Time, ts runtrace.TurnState, ok bool, why string) {
	s.send(command{kind: cmdCloseStep, step: &closeStepReq{now: now, ts: ts, ok: ok, why: why}})
}

// gated reports whether the harness carries operating_hours, reading a staged
// definition first: hours are a supervision key, so a reload that also stages
// a run-affecting change is still seen as gated (or not) at once.
func (s *Supervisor) gated() bool {
	if s.pending != nil {
		return s.pending.OperatingHours != ""
	}
	return s.harness.OperatingHours != ""
}

// shutdownMode and shutdownTimeout read a supervision value from the staged
// definition first, the applied one second, the way gated does. Both apply to
// a close in progress without a restart (SPEC-0012 REQ "Shutdown Mode"), so
// every close decision reads them fresh.
func (s *Supervisor) shutdownMode() core.HoursShutdownMode {
	if s.pending != nil {
		return s.pending.HoursShutdown
	}
	return s.harness.HoursShutdown
}

func (s *Supervisor) shutdownTimeout() time.Duration {
	if s.pending != nil {
		return s.pending.HoursShutdownTimeout
	}
	return s.harness.HoursShutdownTimeout
}

// hoursExpr reads the staged HoursExpr first, the applied one second, the
// same supervision-key contract gated/shutdownMode/shutdownTimeout use. Read
// by the durable-log lines below for the next transition to report (SPEC-0012
// REQ "Operating Hours Visibility") — a presentation-only read, never a gate
// decision, which is still decided from CloseAt/mode/deadline exactly as
// before.
func (s *Supervisor) hoursExpr() hours.Expr {
	if s.pending != nil {
		return s.pending.HoursExpr
	}
	return s.harness.HoursExpr
}

// closeStep is cmdCloseStep on the actor loop: one observation of the
// turn-state watch, decided against the close's conditions. The stop is the
// gate's own (hold steps 3–5 of SPEC-0012 REQ "Gate Enforcement"): no
// restart, no restart-count increment, `enabled` untouched. The durable log
// records which condition ended the close (SPEC-0012 REQ "Graceful
// Shutdown").
func (s *Supervisor) closeStep(req *closeStepReq) {
	if !s.closing || req == nil {
		return // the close was cancelled (release, start, stop) before this step
	}
	now := req.now
	if now.IsZero() {
		now = time.Now()
	}
	mode := s.shutdownMode()
	timeout := s.shutdownTimeout()
	if timeout <= 0 {
		timeout = core.DefaultHoursShutdownTimeout
	}
	deadline := s.closeAt.Add(timeout)

	stop, reason := false, ""
	switch {
	case mode != core.HoursShutdownGraceful:
		// A reload to immediate mid-close applies on the next tick
		// (SPEC-0012 REQ "Shutdown Mode").
		stop, reason = true, "hours_shutdown=immediate"
	case !req.ok:
		// Nothing attributable to the run — a generic adapter, no workdir,
		// or a session correlation excludes: stop at once and say why.
		stop, reason = true, "graceful unavailable: "+req.why
	case req.ts.TurnMarkers && req.ts.TurnEnded && now.Sub(req.ts.LastEventAt) >= closeSettle:
		stop, reason = true, "turn ended"
	case !req.ts.TurnMarkers && now.Sub(req.ts.LastEventAt) >= closeQuiet:
		stop, reason = true, "quiet"
	case !now.Before(deadline):
		stop, reason = true, "deadline reached"
	}
	if !stop {
		s.publishSnapshot()
		return
	}
	s.closing = false
	s.logEvent("close ended", "reason", reason, "deadline", deadline.Format(time.RFC3339))
	s.gracefulStop()
	s.finishRunWith(OutcomeCancelled, &s.lastExitCode, holdRunReason(s.holds))
	s.publishSnapshot()
}

// nextHoursTransition renders the next operating-hours flip for a durable-log
// line (SPEC-0012 REQ "Operating Hours Visibility": "stating the reason and
// the next transition"): the next open while held, the next close once
// released. "unknown" when the expression covers the entire week (no next
// flip exists) or hours were removed from underneath this decision — a log
// line still worth writing, just without a time to give.
func (s *Supervisor) nextHoursTransition() string {
	expr := s.hoursExpr()
	if expr.String() == "" {
		return "unknown"
	}
	_, next, ok := expr.In(time.Now())
	if !ok {
		return "unknown"
	}
	return next.Format(time.RFC3339)
}
