package logview

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

func TestSplitLogFields(t *testing.T) {
	for _, c := range []struct {
		in, msg string
		kvs     []KV
	}{
		{"state changed from=running to=stopping", "state changed", []KV{{"from", "running"}, {"to", "stopping"}}},
		{"run queued", "run queued", nil},
		{`config auto-reload failed (keeping last-good config) err="line 3: bad = value"`, "config auto-reload failed (keeping last-good config)", []KV{{"err", `"line 3: bad = value"`}}},
		{"odd a=b c", "odd a=b c", nil},
	} {
		msg, kvs := SplitLogFields(c.in)
		if msg != c.msg || len(kvs) != len(c.kvs) {
			t.Errorf("SplitLogFields(%q) = %q %v, want %q %v", c.in, msg, kvs, c.msg, c.kvs)
			continue
		}
		for i := range kvs {
			if kvs[i] != c.kvs[i] {
				t.Errorf("SplitLogFields(%q) pair %d = %v, want %v", c.in, i, kvs[i], c.kvs[i])
			}
		}
	}
}

func trueColor() *Style {
	return NewStyle(theme.New(colorprofile.TrueColor, true, theme.DefaultPalette()))
}

func stamp(sec int) string {
	return time.Date(2026, 9, 26, 11, 45, sec, 0, time.Local).Format(time.RFC3339Nano)
}

// TestLinesCollapsesRepeatsLikeTheCLI: the TUI renders a reply through Lines,
// so a repeated call must collapse into one counted line exactly as `harness
// logs` prints it on a terminal.
func TestLinesCollapsesRepeatsLikeTheCLI(t *testing.T) {
	ld := protocol.LogsData{
		Source: protocol.LogSourceAgentTrace,
		Run:    &protocol.LogRun{Start: stamp(0), Adapter: "crush"},
		Entries: []protocol.LogEntry{
			{ID: "a", Time: stamp(31), Kind: protocol.LogEntryTool, Action: "other", Tool: "mcp_signal_send"},
			{ID: "b", Time: stamp(45), Kind: protocol.LogEntryTool, Action: "other", Tool: "mcp_signal_send"},
			{ID: "c", Time: stamp(58), Kind: protocol.LogEntryTool, Action: "read", Target: "/src/internal/mcp/tools.go"},
		},
	}
	got := Lines(ld, trueColor())
	if len(got) != 3 {
		t.Fatalf("got %d lines, want header + collapsed pair + read:\n%s", len(got), strings.Join(got, "\n"))
	}
	if p := ansi.Strip(got[1]); !strings.Contains(p, "×2") || !strings.Contains(p, "mcp_signal_send") {
		t.Errorf("repeat did not collapse: %q", p)
	}
	if p := ansi.Strip(got[2]); !strings.Contains(p, "read      /src/internal/mcp/tools.go") {
		t.Errorf("read line reads %q", p)
	}
	if got[2] == ansi.Strip(got[2]) {
		t.Error("the styled form carries no styling")
	}
}

// TestLinesPlainMatchesFormatEntry: the plain form is the pipe's form, one
// line per entry, uncollapsed.
func TestLinesPlainMatchesFormatEntry(t *testing.T) {
	e := protocol.LogEntry{Time: stamp(1), Kind: protocol.LogEntryTool, Action: "exec", Summary: "go test ./...", Error: true}
	got := Lines(protocol.LogsData{Source: protocol.LogSourceAgentTrace, Entries: []protocol.LogEntry{e, e}}, nil)
	if len(got) != 2 || got[0] != FormatEntry(e) || !strings.HasSuffix(got[0], "(failed)") {
		t.Errorf("plain lines = %q", got)
	}
}

// TestLinesRawSourceStylesDaemonLinesOnly: a reply that is the durable log
// tail keeps the agent's output as it was and re-renders the daemon's lines.
func TestLinesRawSourceStylesDaemonLinesOnly(t *testing.T) {
	ld := protocol.LogsData{Text: "agent says hi\n2026/09/26 11:46:16 INFO exited code=0\n"}
	got := Lines(ld, trueColor())
	if len(got) != 2 || got[0] != "agent says hi" {
		t.Fatalf("raw lines = %q", got)
	}
	if got[1] == "2026/09/26 11:46:16 INFO exited code=0" || ansi.Strip(got[1]) != "2026/09/26 11:46:16 INFO exited code=0" {
		t.Errorf("daemon line = %q", got[1])
	}
}

// TestGroupLineWhoPutsTheSpeakerBeforeTheBadge pins the chatroom's column.
func TestGroupLineWhoPutsTheSpeakerBeforeTheBadge(t *testing.T) {
	g := NewGroup(protocol.LogEntry{Time: stamp(2), Kind: protocol.LogEntryTool, Action: "exec", Summary: "make test"})
	if p := ansi.Strip(trueColor().GroupLineWho(g, "@pr-sweep")); !strings.HasSuffix(p, "  @pr-sweep  exec      make test") {
		t.Errorf("line reads %q", p)
	}
}
