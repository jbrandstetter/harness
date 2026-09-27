package chatroom

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

func at(min, sec int) time.Time { return time.Date(2026, 9, 26, 11, min, sec, 0, time.UTC) }

// sweepRun is a structured reply for one run: a session line, lifecycle, two
// tool calls and a user prompt.
func sweepRun(session string, base int) protocol.LogsData {
	ts := func(sec int) string { return at(base, sec).Format(time.RFC3339Nano) }
	return protocol.LogsData{
		Source: protocol.LogSourceAgentTrace,
		Run:    &protocol.LogRun{Start: ts(0)},
		Entries: []protocol.LogEntry{
			{ID: "lc", Time: ts(0), Kind: protocol.LogEntryLifecycle, Action: "state", Summary: "stopped → running"},
			{ID: session + "/session", Time: ts(1), Kind: protocol.LogEntrySession, Action: "session", Session: session},
			{ID: session + "/mark/0", Time: ts(2), Kind: protocol.LogEntryMark, Action: "user", Summary: "sweep the backlog", Session: session},
			{ID: session + "/tool/0", Time: ts(3), Kind: protocol.LogEntryTool, Action: "read", Tool: "view", Target: "/src/app/main.go", Session: session},
			{ID: session + "/tool/1", Time: ts(4), Kind: protocol.LogEntryTool, Action: "exec", Tool: "bash", Summary: "go test ./...", Session: session, Error: true},
		},
	}
}

func plainLines(m *Model) []string {
	var out []string
	for _, l := range m.Buffer().Lines(m.styles) {
		out = append(out, ansi.Strip(l))
	}
	return out
}

// TestAddHistoryFillsAnEmptyChatroom is the report: with nothing live, the
// chatroom opened empty. History merges each harness's latest run, attributed
// to it, agent activity only.
func TestAddHistoryFillsAnEmptyChatroom(t *testing.T) {
	m := New(theme.Default(), nil)
	m.AddHistory("pr-sweep", "crush", sweepRun("s1", 40))
	m.AddHistory("arr-sweep", "claude-code", sweepRun("s2", 30))

	got := plainLines(m)
	if len(got) != 6 {
		t.Fatalf("got %d rows, want 3 per run (prompt, read, exec):\n%s", len(got), strings.Join(got, "\n"))
	}
	// Merged by time: the 11:30 run first, whoever was fetched first.
	if !strings.Contains(got[0], "@arr-sweep") || !strings.Contains(got[3], "@pr-sweep") {
		t.Errorf("rows are not merged in time order under their harness:\n%s", strings.Join(got, "\n"))
	}
	for _, want := range []string{"prompt    sweep the backlog", "read      /src/app/main.go", "exec      go test ./...  (failed)"} {
		if !strings.Contains(got[3]+"\n"+got[4]+"\n"+got[5], want) {
			t.Errorf("rows lack %q:\n%s", want, strings.Join(got, "\n"))
		}
	}
	for _, l := range got {
		if strings.Contains(l, "stopped") || strings.Contains(l, "session") {
			t.Errorf("a lifecycle or session entry reached the chatroom: %q", l)
		}
	}
}

// TestAddHistoryRowsReadLikeHarnessLogs: a chatroom row is the `harness logs`
// line with the speaker between the clock and the label.
func TestAddHistoryRowsReadLikeHarnessLogs(t *testing.T) {
	m := New(theme.Default(), nil)
	run := sweepRun("s1", 40)
	m.AddHistory("pr-sweep", "crush", run)
	got := plainLines(m)
	e := run.Entries[4]
	f := logview.Parts(e)
	want := f.Clock + "  " + "@pr-sweep" + strings.Repeat(" ", whoWidth-len("@pr-sweep")) + "  " + ansi.Strip(logview.FormatEntry(e))[len(f.Clock)+2:]
	if got[2] != want {
		t.Errorf("row reads\n  %q\nwant\n  %q", got[2], want)
	}
}

// TestAddHistoryDedupesAgainstTheWatcher: anything recent arrives from both
// the watcher and the daemon, and must show once whichever came first — and
// a poll that re-delivers the same run adds nothing.
func TestAddHistoryDedupesAgainstTheWatcher(t *testing.T) {
	live := tail.Event{
		Session: tail.SessionMeta{ID: "s1", Harness: tail.HarnessCrush},
		Classified: classify.Event{
			Seq: 1, Timestamp: at(40, 4).Format(time.RFC3339), Tool: "bash",
			Action: classify.ActionExec, Summary: "go test ./... -> 0 targets, 0 outside error", IsError: true,
		},
		Marks: []classify.Mark{{Seq: 1, Timestamp: at(40, 2).Format(time.RFC3339), Type: "user", Note: "sweep the backlog"}},
	}
	for _, order := range []string{"watcher first", "history first"} {
		t.Run(order, func(t *testing.T) {
			m := New(theme.Default(), nil)
			if order == "watcher first" {
				m.Add(live)
				m.AddHistory("pr-sweep", "crush", sweepRun("s1", 40))
			} else {
				m.AddHistory("pr-sweep", "crush", sweepRun("s1", 40))
				m.Add(live)
			}
			m.AddHistory("pr-sweep", "crush", sweepRun("s1", 40))
			if got := plainLines(m); len(got) != 3 {
				t.Fatalf("got %d rows, want prompt, read, exec once each:\n%s", len(got), strings.Join(got, "\n"))
			}
		})
	}
}

// TestAddHistoryNamesTheLiveSession: the daemon attributed the session with
// the harness's own stores, so its live events speak as that harness too —
// even where the attributor, which cannot see those stores, credits no one.
func TestAddHistoryNamesTheLiveSession(t *testing.T) {
	m := New(theme.Default(), nil)
	m.SetAttributor(func(tail.SessionMeta) string { return "" })
	m.Add(tail.Event{
		Session:    tail.SessionMeta{ID: "s1", Harness: tail.HarnessCrush},
		Classified: classify.Event{Seq: 7, Timestamp: at(41, 0).Format(time.RFC3339), Tool: "bash", Action: classify.ActionExec, Summary: "git push"},
	})
	if got := plainLines(m); !strings.Contains(got[0], "@crush") {
		t.Fatalf("unattributed live row reads %q", got[0])
	}
	m.AddHistory("pr-sweep", "crush", sweepRun("s1", 40))
	for _, l := range plainLines(m) {
		if !strings.Contains(l, "@pr-sweep") {
			t.Errorf("row not relabelled to its harness: %q", l)
		}
	}
}

// TestAddHistoryIgnoresTheRawTail: a daemon that cannot build the activity
// view answers with the durable log, which has no events to merge.
func TestAddHistoryIgnoresTheRawTail(t *testing.T) {
	m := New(theme.Default(), nil)
	m.AddHistory("old", "crush", protocol.LogsData{Text: "2026/09/26 11:46:16 INFO exited code=0\n"})
	if n := m.Buffer().Len(); n != 0 {
		t.Fatalf("raw tail filed %d events", n)
	}
}

// TestAddHistorySkipsAmbiguousSessions: a session more than one harness could
// have written is never credited to one (SPEC-0006).
func TestAddHistorySkipsAmbiguousSessions(t *testing.T) {
	run := sweepRun("s1", 40)
	for i := range run.Entries {
		run.Entries[i].Ambiguous = true
	}
	m := New(theme.Default(), nil)
	m.AddHistory("pr-sweep", "crush", run)
	if n := m.Buffer().Len(); n != 0 {
		t.Fatalf("ambiguous entries filed %d events", n)
	}
}
