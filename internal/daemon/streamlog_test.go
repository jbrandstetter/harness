package daemon

// Pipe-Run Streams In The Logs View — Tests
//
// Governing: ADR-0033 "Structured one-shots run on pipes", "What reads the
// records"; SPEC-0017 REQ-18; SPEC-0008 REQ "Per-Run Logs".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/testwait"
)

// pipeOneShot is a scheduled claude-code prompt one-shot; with a fake
// `claude` first on PATH it runs on pipes exactly as a real one would.
func pipeOneShot(name, workdir string) core.Harness {
	h := scheduledSh(name, "", workdir)
	h.Adapter, h.Args, h.Prompt = "claude-code", nil, "triage the queue"
	return h
}

// fakeClaude puts a `claude` running the sh script body first on PATH.
func fakeClaude(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// `harness logs NAME --run N --raw` on a finished pipe run prints the stream,
// masked, and then the run log with the agent's stderr, each under a header;
// the durable log's raw tail says where the stdout went.
func TestLogsRunShowsAPipeRunsStream(t *testing.T) {
	fakeClaude(t, `echo '{"type":"system","subtype":"init"}'
echo 'warning: from stderr' >&2
echo '{"type":"tool_use","input":{"command":"GITEA_TOKEN=abc123 tea pr ls"}}'
echo '{"type":"result","subtype":"success"}'
`)
	td, _, _ := newJobsDaemon(t, pipeOneShot("sweep", t.TempDir()))
	c := td.dial(t, nil)
	if _, err := c.Trigger("sweep"); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "sweep", finishedN(1))

	ld, err := c.RunLogs("sweep", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	text := ld.Text
	iStream := strings.Index(text, ".stream.jsonl <==")
	iInit := strings.Index(text, `{"type":"system","subtype":"init"}`)
	iResult := strings.Index(text, `{"type":"result","subtype":"success"}`)
	iLog := strings.Index(text, "/1.log <==")
	iWarn := strings.Index(text, "warning: from stderr")
	if iStream < 0 || iInit < iStream || iResult < iInit || iLog < iResult || iWarn < iLog {
		t.Fatalf("want the stream (init … result) then the run log (stderr), each under a header:\n%s", text)
	}
	if strings.Contains(text, "abc123") || !strings.Contains(text, "GITEA_TOKEN=[REDACTED]") {
		t.Errorf("the stream's credential is not masked:\n%s", text)
	}

	raw, err := c.Logs("sweep", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.Text, `"type":"system"`) {
		t.Errorf("the durable log carries stdout:\n%s", raw.Text)
	}
	if len(raw.Notices) == 0 || !strings.Contains(raw.Notices[0], "--run N --raw") {
		t.Errorf("the durable log's reply does not say where stdout went: %q", raw.Notices)
	}
}

// While a pipe run is live its text is the stream alone, so a `trigger
// --wait` that prints each poll's new suffix sees the stream grow, then the
// run log appended once, at the end: every poll's text extends the last.
func TestLogsRunPipeTextOnlyGrows(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	fakeClaude(t, `echo '{"type":"system"}'
echo 'early stderr' >&2
while [ ! -e '`+gate+`' ]; do sleep 0.02; done
echo '{"type":"result"}'
`)
	td, _, _ := newJobsDaemon(t, pipeOneShot("sweep", dir))
	c := td.dial(t, nil)
	if _, err := c.Trigger("sweep"); err != nil {
		t.Fatal(err)
	}
	var live string
	deadline := time.Now().Add(testwait.Budget(t, 5*time.Second))
	for !strings.Contains(live, `{"type":"system"}`) {
		if time.Now().After(deadline) {
			t.Fatalf("the live run's stream never showed:\n%s", live)
		}
		ld, err := c.RunLogs("sweep", 1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		live = ld.Text
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(live, "early stderr") {
		t.Errorf("a live run's text carries its run log, which would not stay a prefix:\n%s", live)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "sweep", finishedN(1))
	ld, err := c.RunLogs("sweep", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ld.Text, live) || !strings.Contains(ld.Text[len(live):], "early stderr") {
		t.Errorf("the finished text does not extend the live text with the run log:\nlive:\n%s\nfinished:\n%s", live, ld.Text)
	}
}

// streamTail bounds what it returns: n lines, each cut at lineMax with a note,
// the oldest dropped past budget.
func TestStreamTailBounds(t *testing.T) {
	in := "a\n" + strings.Repeat("b", 100) + "\nc\nd"
	got := streamTail(strings.NewReader(in), 3, 10, 1<<20)
	want := []string{strings.Repeat("b", 10) + " …[90 more bytes; the whole line is in the stream file]", "c", "d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("streamTail = %q, want %q", got, want)
	}
	got = streamTail(strings.NewReader("one\ntwo\nthree\n"), 0, 100, 9)
	if strings.Join(got, "|") != "two|three" {
		t.Errorf("budgeted tail = %q, want the newest lines within 9 bytes", got)
	}
}
