package main

// Agent Activity Logs
//
// Renders the structured view of `harness logs`: the latest run of a harness as
// a time-ordered list of lifecycle lines and the agent-trace events attributed
// to that run — what the agent read, ran, edited, and the error it died on —
// instead of the PTY history of a full-screen TUI. A harness with no native
// transcript, or a daemon too old to build the view, answers with the durable
// log tail and it prints exactly as `--raw` does.
//
// Every string here came out of an agent transcript, and a transcript records
// whatever a model or a tool printed. Each field is flattened to one line with
// control characters removed before it reaches the terminal (#146).
//
// Governing: SPEC-0002 REQ "Control Operations" ("logs"), SPEC-0006 REQ "Run
// Correlation", issue #302.
//
// @joestump-agent 09/11/2026 - Added for harness#302.
//
// @joestump-agent 09/27/2026 - The entry, run and notice renderers moved to
// internal/logview, shared with the TUI's one-shot preview and the chatroom.

import (
	"io"
	"time"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
)

// activityPoll is the --follow re-fetch interval, matching the agent-trace
// watcher's own poll.
const activityPoll = 2 * time.Second

// cmdLogEvents prints one structured logs reply, or follows the run.
func cmdLogEvents(c *client.Client, o verbOpts, w io.Writer) error {
	fetch := func(lines int) (protocol.LogsData, error) {
		return c.LogEvents(o.name, client.LogOptions{Lines: lines, IncludeAmbiguous: o.ambiguous, Run: o.run})
	}
	if o.follow && !o.json {
		ld, err := fetch(o.lines)
		if err != nil {
			return err
		}
		if ld.Source != protocol.LogSourceAgentTrace {
			return followLogs(c, o)
		}
		v := newActivityView(w, logStyleFor(w))
		v.live = true
		return followActivity(v, ld, fetch, o.lines, func() bool {
			time.Sleep(activityPoll)
			return true
		})
	}
	ld, err := fetch(o.lines)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(ld)
	}
	newActivityView(w, logStyleFor(w)).render(ld)
	return nil
}

// renderActivity prints a logs reply as plain text: the exact bytes a pipe, a
// script or an agent reads. A reply that is not the activity view is the
// durable log tail and prints as such.
func renderActivity(w io.Writer, ld protocol.LogsData) {
	newActivityView(w, nil).render(ld)
}

// render prints one logs reply through the view.
func (v *activityView) render(ld protocol.LogsData) {
	if ld.Source != protocol.LogSourceAgentTrace {
		v.flush()
		writeLogText(newRawWriter(v.w, v.st), ld.Text)
		return
	}
	if ld.Run != nil {
		v.header(*ld.Run)
	}
	for _, n := range ld.Notices {
		v.note(n)
	}
	for _, e := range ld.Entries {
		v.entry(e)
	}
	v.flush()
	// The activity view never prints the durable log, even if a daemon sent
	// one: this view is agent activity, and the stored history of a
	// full-screen agent is its repainted screen. `--raw` is the way to read
	// the log, and the daemon's notices say so (#279).
}

// followActivity prints first, then re-fetches until wait reports false,
// printing each entry the first time it appears. A new run (the harness
// restarted, or a schedule fired) prints its own header.
//
// Re-fetches ask for four times the line budget: more than that arriving within
// one poll interval is not a rate an agent runs tools at, and the entries' IDs
// make the overlap harmless.
//
// The view carries the styling: a plain view prints exactly what a pipe has
// always read, a styled one collapses repeats in place as they arrive.
func followActivity(v *activityView, first protocol.LogsData, fetch func(lines int) (protocol.LogsData, error), lines int, wait func() bool) error {
	v.render(first)
	seen := map[string]bool{}
	for _, e := range first.Entries {
		seen[e.ID] = true
	}
	run := ""
	if first.Run != nil {
		run = first.Run.Start
	}
	for wait() {
		ld, err := fetch(lines * 4)
		if err != nil {
			return err
		}
		if ld.Run != nil && ld.Run.Start != run {
			run = ld.Run.Start
			v.header(*ld.Run)
		}
		for _, e := range ld.Entries {
			if seen[e.ID] {
				continue
			}
			seen[e.ID] = true
			v.entry(e)
		}
		v.flush()
	}
	return nil
}

// The renderer itself is shared with the TUI (internal/logview); these names
// keep this file and its tests reading as they always have.
var (
	runHeader   = logview.RunHeader
	noteLine    = logview.NoteLine
	formatEntry = logview.FormatEntry
	inert       = logview.Inert
)
