package adapter

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// The oracle for these shapes is Claude Code's own stream-json output, as
// documented in issue #13: heartbeats arrive as tool_progress/task_progress
// events carrying heartbeat:true, assistant messages carry text and tool_use
// content blocks, and the run ends with a result object.

func renderAll(t *testing.T, chunks ...string) string {
	t.Helper()
	r := &streamJSONRenderer{}
	var out bytes.Buffer
	for _, c := range chunks {
		out.Write(r.FormatPTY([]byte(c)))
	}
	return out.String()
}

func TestStreamJSONHeartbeatsCollapseToOneLine(t *testing.T) {
	hb := func(n int, elapsed float64) string {
		return `{"type":"tool_progress","tool_use_id":"u-` + strconv.Itoa(n) + `","tool_name":"Agent","heartbeat":true,"elapsed_time_seconds":` +
			strconv.FormatFloat(elapsed, 'f', -1, 64) + `}` + "\n"
	}

	got := renderAll(t, hb(1, 30), hb(2, 60), hb(3, 90))
	if strings.Count(got, "heartbeat") != 3 {
		t.Fatalf("each ping leaked its own line, not one live tally:\n%q", got)
	}
	if !strings.HasPrefix(got, "[1 heartbeat over 30s]") {
		t.Errorf("first ping did not paint the tally:\n%q", got)
	}
	// Second and third pings rewrite in place: \r then the new text (the
	// tally only ever grows here, so no padding is needed).
	want2 := "\r[2 heartbeats over 1m00s]"
	if !strings.Contains(got, want2) {
		t.Errorf("second ping did not overwrite the tally:\nwant %q in\n%q", want2, got)
	}
	want3 := "\r[3 heartbeats over 1m30s]"
	if !strings.Contains(got, want3) {
		t.Errorf("third ping did not overwrite the tally:\nwant %q in\n%q", want3, got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("the tally line must hold the cursor (no trailing newline) while the run is live:\n%q", got)
	}
}

func TestStreamJSONTallyClosesBeforeNextContent(t *testing.T) {
	got := renderAll(t,
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":30}`+"\n",
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":60}`+"\n",
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`+"\n",
	)
	want := "[1 heartbeat over 30s]\r[2 heartbeats over 1m00s]\r\nDone.\r\n"
	if got != want {
		t.Errorf("tally did not close cleanly before the next line:\n got %q\nwant %q", got, want)
	}
	// A fresh run starts a fresh tally at [1 heartbeat], not a resumed one.
	got = renderAll(t, got,
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":10}`+"\n",
	)
	if !strings.Contains(got, "\r\n[1 heartbeat over 10s]") {
		t.Errorf("a second run did not start its own tally:\n%q", got)
	}
}

func TestStreamJSONToolUseLeadsWithDescription(t *testing.T) {
	got := renderAll(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git push origin main","description":"Push branch to GitHub"}}]}}`+"\n")
	if got != "▸ Push branch to GitHub\r\n" {
		t.Errorf("tool_use did not lead with its description:\n%q", got)
	}
}

func TestStreamJSONToolUseFallsBackToName(t *testing.T) {
	got := renderAll(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"prompt":"do things"}}]}}`+"\n")
	if got != "▸ Agent\r\n" {
		t.Errorf("tool_use without a description did not fall back to the tool name:\n%q", got)
	}
}

func TestStreamJSONTextBlocksPlain(t *testing.T) {
	got := renderAll(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at the failing test now.\n"},{"type":"text","text":"Second thought."}]}}`+"\n")
	want := "Looking at the failing test now.\r\nSecond thought.\r\n"
	if got != want {
		t.Errorf("text blocks did not render as plain text:\n got %q\nwant %q", got, want)
	}
}

func TestStreamJSONResultSummary(t *testing.T) {
	got := renderAll(t, `{"type":"result","subtype":"success","is_error":false,"duration_ms":41230,"num_turns":12,"total_cost_usd":0.41}`+"\n")
	want := "✓ result · success · 12 turns · 41s · $0.41\r\n"
	if got != want {
		t.Errorf("result line:\n got %q\nwant %q", got, want)
	}
	got = renderAll(t, `{"type":"result","subtype":"error_max_turns","is_error":true}`+"\n")
	if !strings.HasPrefix(got, "✖ result · error_max_turns") {
		t.Errorf("failed result line:\n%q", got)
	}
}

func TestStreamJSONNoiseSkipped(t *testing.T) {
	got := renderAll(t,
		`{"type":"system","subtype":"init","model":"claude-opus-5"}`+"\n",
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"..."}]}}`+"\n",
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`+"\n",
	)
	if got != "" {
		t.Errorf("noise events leaked into the preview:\n%q", got)
	}
}

func TestStreamJSONNonJSONPassthrough(t *testing.T) {
	raw := "bash: line 1: foo: command not found\n\x1b[31mred escape output\x1b[0m\n"
	want := "bash: line 1: foo: command not found\r\n\x1b[31mred escape output\x1b[0m\r\n"
	if got := renderAll(t, raw); got != want {
		t.Errorf("non-JSON terminal output was not passed through byte-for-byte:\n got %q\nwant %q", got, raw)
	}
}

func TestStreamJSONUnparseableJSONPassthrough(t *testing.T) {
	raw := "{not json at all\n"
	if got := renderAll(t, raw); got != "{not json at all\r\n" {
		t.Errorf("unparseable JSON was not passed through:\n got %q\nwant %q", got, raw)
	}
}

func TestStreamJSONUnknownEventKindPassthrough(t *testing.T) {
	raw := `{"type":"control_request","request_id":"x","request":{"subtype":"can_use_tool"}}` + "\n"
	want := `{"type":"control_request","request_id":"x","request":{"subtype":"can_use_tool"}}` + "\r\n"
	if got := renderAll(t, raw); got != want {
		t.Errorf("unmodeled event kind was not passed through:\n got %q\nwant %q", got, raw)
	}
}

func TestStreamJSONLineSplitAcrossChunks(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"description":"Split across reads"}}]}}`
	got := renderAll(t, line[:20], line[20:60], line[60:]+"\n")
	if got != "▸ Split across reads\r\n" {
		t.Errorf("a line split across chunks did not render once:\n%q", got)
	}
}

func TestStreamJSONCRLEFPassthrough(t *testing.T) {
	raw := "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\r\n"
	if got := renderAll(t, raw); got != "hi\r\n" {
		t.Errorf("CRLF line was not normalized:\n%q", got)
	}
}

// The PTY terminates each guest line with CRLF and the VT emulator treats a
// bare LF as "down one row, no carriage return", so every byte the formatter
// emits must carry its CR (#877).
func TestStreamJSONFormatPTYNeverEmitsBareLF(t *testing.T) {
	in := "bash: line 1: foo: command not found\r\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"First line.\nSecond line."}]}}\r\n` +
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls","description":"List files"}}]}}\r\n` +
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":30}\r\n` +
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"num_turns":1,"total_cost_usd":0.01}\r\n`
	got := renderAll(t, in)
	for i := 1; i < len(got); i++ {
		if got[i] == '\n' && got[i-1] != '\r' {
			t.Fatalf("a bare \\n reached the emulator; every LF must be a CRLF pair:\n%q", got)
		}
	}
}

// inkRedraw is the shape of an interactive claude session's output, cut down
// from a recorded trust dialog: Ink paints a frame inside synchronized-output
// markers and, on every keypress, redraws it with relative cursor moves
// (\x1b[nA up, \x1b[nG column) that count rows, blank ones included. The
// frame ends on a cursor-parking sequence with no newline after it.
const inkFrame = "\x1b[?2026h" +
	"Do you trust this folder?\r\r\n" +
	"\r\r\n" +
	"\x1b[2G\u276f\x1b[4G1. No, exit\r\r\n" +
	"\x1b[4G2. Yes, trust\r\r\n" +
	"\r\r\n" +
	"Enter to confirm" +
	"\x1b[?2026l"

const inkRedraw = "\x1b[?2026h" +
	"\x1b[5A\r\x1b[J" +
	"Do you trust this folder?\r\n" +
	"\r\n" +
	"\x1b[4G1. No, exit\r\n" +
	"\x1b[2G\u276f\x1b[4G2. Yes, trust\r\n" +
	"\r\n" +
	"Enter to confirm" +
	"\x1b[1C\x1b[3A\x1b[?2026l"

// An interactive claude session is not stream-json. Its bytes must come out
// exactly as they went in, and without waiting for a newline: dropping the
// blank rows or withholding the cursor-parking tail leaves the emulator
// counting rows against a screen that is no longer the one Ink drew.
func TestStreamJSONInteractiveBytesPassThroughVerbatim(t *testing.T) {
	in := inkFrame + inkRedraw
	for split := 0; split <= len(in); split++ {
		if split > 0 && split < len(in) && in[split-1] == '\r' && in[split] == '\n' {
			// onlcr keeps no state between chunks, so a CRLF cut in half
			// gains a redundant CR. Harmless to an emulator, and not what
			// this test is about.
			continue
		}
		r := &streamJSONRenderer{}
		var out []byte
		for _, c := range []string{in[:split], in[split:]} {
			got := r.FormatPTY([]byte(c))
			if len(got) != len(c) {
				t.Fatalf("split at %d: %d bytes in, %d out; a chunk without a newline must not be held or reshaped:\n in %q\nout %q", split, len(c), len(got), c, got)
			}
			out = append(out, got...)
		}
		if string(out) != in {
			t.Fatalf("split at %d: bytes changed in transit:\n got %q\nwant %q", split, out, in)
		}
	}
}

// A terminal line that merely starts with "{" is held only until it is
// recognised as terminal output: JSON cannot contain a raw escape byte, so
// the first one releases the line.
func TestStreamJSONBraceLedTerminalOutputIsReleased(t *testing.T) {
	r := &streamJSONRenderer{}
	in := "{ \x1b[1mbold\x1b[0m cursor parked\x1b[3A"
	if got := r.FormatPTY([]byte(in)); string(got) != in {
		t.Errorf("a brace-led line carrying an escape byte was held back:\n got %q\nwant %q", got, in)
	}
	if r.FormatPTY([]byte("more\r\n")); len(r.buf) != 0 || r.raw {
		t.Errorf("renderer did not return to line start after the line ended: buf=%q raw=%v", r.buf, r.raw)
	}
}

// The line buffer's cap releases an endless brace-led line and keeps
// forwarding the rest of it, rather than buffering it again.
func TestStreamJSONBufferCapReleasesTheRestOfTheLine(t *testing.T) {
	r := &streamJSONRenderer{}
	head := "{" + strings.Repeat("x", streamJSONLineCap)
	if got := r.FormatPTY([]byte(head)); string(got) != head {
		t.Fatalf("oversized brace-led chunk was not released: %d bytes in, %d out", len(head), len(got))
	}
	if got := r.FormatPTY([]byte("tail")); string(got) != "tail" {
		t.Errorf("the rest of a released line was held again:\n%q", got)
	}
}

func TestStreamJSONBufferCapFlushesVerbatim(t *testing.T) {
	r := &streamJSONRenderer{}
	big := strings.Repeat("x", streamJSONLineCap+16) // no newline anywhere
	got := r.FormatPTY([]byte(big))
	if string(got) != big {
		t.Errorf("oversized newline-less chunk was not flushed verbatim: %d bytes in, %d out", len(big), len(got))
	}
	if len(r.buf) != 0 {
		t.Errorf("line buffer did not reset after the cap flush: %d bytes held", len(r.buf))
	}
}

func TestStreamJSONTallySurvivesSplitHeartbeatLine(t *testing.T) {
	// A heartbeat whose JSON arrives in two reads still tallies once.
	got := renderAll(t,
		`{"type":"tool_progress","heartbeat":true,"elap`,
		`sed_time_seconds":45}`+"\n",
	)
	if !strings.HasPrefix(got, "[1 heartbeat over 45s]") {
		t.Errorf("split heartbeat line did not paint the tally:\n%q", got)
	}
}

func TestPeekFormatterFor(t *testing.T) {
	if PeekFormatterFor("claude-code") == nil {
		t.Error("claude-code must provide a peek formatter (issue #13)")
	}
	for _, name := range []string{"crush", "codex", "generic", "command", "no-such-adapter"} {
		if PeekFormatterFor(name) != nil {
			t.Errorf("%q must keep the preview byte-faithful (no formatter), got one", name)
		}
	}
}
