package tui

// Coverage for the one-shot preview: a prompt harness's pane renders the
// structured `logs` reply the way `harness logs` prints it, an interactive
// harness keeps the PTY mirror, and a daemon that cannot build the activity
// view leaves the historical preview in place.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
)

// collect runs cmd and every command it batches, returning the messages they
// produce — so a test can feed them back through Update, as the runtime does.
func collect(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, collect(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// oneShotActivity is a structured reply shaped like a crush sweep's run.
func oneShotActivity(name string) protocol.LogsData {
	at := func(sec int) string {
		return time.Date(2026, 9, 26, 11, 45, sec, 0, time.Local).Format(time.RFC3339Nano)
	}
	return protocol.LogsData{
		Name:   name,
		Text:   "the agent's headless prose, which the preview used to mirror\n",
		Source: protocol.LogSourceAgentTrace,
		Run:    &protocol.LogRun{Start: at(0), Adapter: "crush"},
		Entries: []protocol.LogEntry{
			{ID: "1", Time: at(4), Kind: protocol.LogEntryTool, Action: "exec", Summary: "go vet ./..."},
			{ID: "2", Time: at(31), Kind: protocol.LogEntryTool, Action: "other", Tool: "mcp_signal_send"},
			{ID: "3", Time: at(45), Kind: protocol.LogEntryTool, Action: "other", Tool: "mcp_signal_send"},
		},
	}
}

// oneShotModel is peekModel with the selection made a prompt harness, and a
// controller answering the structured fetch with reply.
func oneShotModel(t *testing.T, reply func(string) protocol.LogsData) (*Model, *fakeController) {
	t.Helper()
	m, _ := peekModel()
	m.harnesses[0].Prompt = "sweep the PR backlog"
	fc := m.ctrl.(*fakeController)
	fc.events = reply
	return m, fc
}

func pump(m *Model, cmd tea.Cmd) {
	for _, msg := range collect(cmd) {
		m.Update(msg)
	}
}

func TestPeekRendersOneShotAsItsActivity(t *testing.T) {
	m, fc := oneShotModel(t, oneShotActivity)
	pump(m, m.peekCmd())

	if len(fc.eventCalls) != 1 {
		t.Fatalf("structured fetches = %v, want one for the selected one-shot", fc.eventCalls)
	}
	got := ansi.Strip(m.viewPeek(120, m.bodyHeight()))
	for _, want := range []string{"run activity", "exec      go vet ./...", "×2", "mcp_signal_send", "run 2026-09-26 11:45:00"} {
		if !strings.Contains(got, want) {
			t.Errorf("preview lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "headless prose") {
		t.Errorf("preview mirrored the PTY tail instead of the activity:\n%s", got)
	}
}

// TestPeekOneShotMatchesHarnessLogs is the requirement itself: every activity
// line in the pane is a line `harness logs` would print for the same reply.
func TestPeekOneShotMatchesHarnessLogs(t *testing.T) {
	m, _ := oneShotModel(t, oneShotActivity)
	pump(m, m.peekCmd())

	pane := m.viewPeek(160, m.bodyHeight())
	for _, line := range logview.Lines(oneShotActivity("x"), logview.NewStyle(m.theme)) {
		if !strings.Contains(pane, line) {
			t.Errorf("pane lacks the `harness logs` line %q", ansi.Strip(line))
		}
	}
}

func TestPeekInteractiveHarnessNeverFetchesActivity(t *testing.T) {
	m, _ := peekModel()
	fc := m.ctrl.(*fakeController)
	pump(m, m.peekCmd())
	if len(fc.eventCalls) != 0 {
		t.Fatalf("an interactive harness fetched activity: %v", fc.eventCalls)
	}
	if got := ansi.Strip(m.viewPeek(120, m.bodyHeight())); !strings.Contains(got, "live preview") {
		t.Errorf("interactive preview lost its PTY mirror:\n%s", got)
	}
}

// TestPeekOneShotFallsBackToTheTail: a daemon that answers the structured
// request with the raw tail (too old, or no transcript) keeps the preview the
// operator always had.
func TestPeekOneShotFallsBackToTheTail(t *testing.T) {
	m, _ := oneShotModel(t, nil) // fakeController answers with a raw tail
	pump(m, m.peekCmd())
	got := ansi.Strip(m.viewPeek(120, m.bodyHeight()))
	if strings.Contains(got, "run activity") || !strings.Contains(got, "log line") {
		t.Errorf("fallback preview reads:\n%s", got)
	}
}

func TestPeekActivityPollIsThrottled(t *testing.T) {
	m, fc := oneShotModel(t, oneShotActivity)
	pump(m, m.peekCmd())
	pump(m, m.peekCmd())
	if len(fc.eventCalls) != 1 {
		t.Fatalf("two polls inside %v issued %d structured fetches, want 1", peekActivityPoll, len(fc.eventCalls))
	}
	// A request still in flight is never doubled, even once the interval is up.
	m.peekAct.fetchAt = time.Now().Add(-peekActivityPoll)
	m.peekAct.inflight = true
	if cmd := m.peekActivityCmd(m.harnesses[0]); cmd != nil {
		t.Fatal("a second fetch was issued while the first was unanswered")
	}
}

// TestPeekActivityIsPerHarness: a reply for a harness no longer selected never
// lands in the pane, and the next one-shot fetches at once.
func TestPeekActivityIsPerHarness(t *testing.T) {
	m, fc := oneShotModel(t, oneShotActivity)
	m.harnesses[1].Prompt = "another sweep"
	stale := collect(m.peekCmd())

	m.moveSel(1)
	for _, msg := range stale {
		m.Update(msg)
	}
	sel, _ := m.selectedHarness()
	if _, ok := m.peekActivityLines(sel); ok {
		t.Fatal("the previous selection's activity rendered under the new one")
	}
	pump(m, m.peekCmd())
	if len(fc.eventCalls) != 2 || fc.eventCalls[1] != sel.Name {
		t.Fatalf("structured fetches = %v, want the new selection fetched at once", fc.eventCalls)
	}
}
