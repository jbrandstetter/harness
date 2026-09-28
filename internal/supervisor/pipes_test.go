package supervisor

// Structured One-Shots On Pipes: Tests
//
// Every test here drives a real Manager and a fake `claude` first on PATH. The
// claude-code adapter declares a structured stream, so the fake is spawned
// exactly as a real stream-json one-shot would be, argv and all. Nothing is
// special-cased for tests: there is no escape hatch that puts an arbitrary
// command on pipes (ADR-0033).
//
// Governing: ADR-0033 "Structured one-shots run on pipes" and "Confirmation"
// (the process has no controlling terminal; the stream file parses line by
// line); SPEC-0017 REQ-18; SPEC-0008 REQ "Per-Run Logs".

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/harness/internal/core"
)

// fakeAgent puts an executable named exe, running the sh script body, first
// on PATH.
func fakeAgent(t *testing.T, exe, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, exe), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// pipeSweep is a scheduled claude-code prompt one-shot: a structured stream,
// so it runs on pipes.
func pipeSweep(name string) core.Harness {
	h := sweep(name, "")
	h.Adapter, h.Args = "claude-code", nil
	h.Prompt = "triage the queue"
	return h
}

// ttyProbe is a script fragment writing, to path, the process's controlling
// terminal (tty_nr from /proc, else ps's TTY column) and which of its standard
// descriptors are terminals.
func ttyProbe(path string) string {
	return fmt.Sprintf(`{ if [ -r /proc/$$/stat ]; then cut -d' ' -f7 /proc/$$/stat; else ps -o tty= -p $$; fi
[ -t 0 ] && echo stdin-tty; [ -t 1 ] && echo stdout-tty; [ -t 2 ] && echo stderr-tty; } > %q
`, path)
}

// noTerminal reports whether a ttyProbe record says "no controlling terminal
// and no terminal descriptors".
func noTerminal(probe string) bool {
	lines := strings.Fields(probe)
	if len(lines) != 1 {
		return false // a descriptor was a terminal
	}
	switch lines[0] {
	case "0", "?", "??":
		return true
	}
	return false
}

// A stream-json one-shot has no controlling terminal and no terminal on any
// descriptor. The control runs the same probe as a crush one-shot, which
// keeps its PTY, and must see one: the probe can tell the difference.
func TestPipeRunHasNoTerminal(t *testing.T) {
	e := newRunsEnv(t)
	pipeProbe := filepath.Join(e.dir, "pipe.tty")
	ptyProbe := filepath.Join(e.dir, "pty.tty")
	fakeAgent(t, "claude", ttyProbe(pipeProbe)+`echo '{"type":"result"}'`+"\n")
	fakeAgent(t, "crush", ttyProbe(ptyProbe))

	control := sweep("control", "")
	control.Adapter, control.Args, control.Prompt = "crush", nil, "x"
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot"), control), fastPolicy())

	m.StartRun("control", RunRequest{Trigger: TriggerManual})
	waitRuns(t, m, "control", "control run ends", outcomesAre(OutcomeSuccess))
	if got := readText(t, ptyProbe); noTerminal(got) {
		t.Fatalf("the PTY control saw no terminal (%q), so the probe cannot detect one", got)
	}

	m.StartRun("oneshot", RunRequest{Trigger: TriggerManual})
	waitRuns(t, m, "oneshot", "pipe run ends", outcomesAre(OutcomeSuccess))
	if got := readText(t, pipeProbe); !noTerminal(got) {
		t.Errorf("the stream-json one-shot had a terminal: probe %q", got)
	}
}

// streamScript is a fake claude that writes a stream with a credential inside
// a tool call and a multi-megabyte line, two stderr lines (one carrying a
// credential), and exits 3.
const streamScript = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-1"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -H \"Authorization: token abc123\" https://x"}}]}}'
echo 'warning: stderr line one' >&2
printf '{"type":"user","content":"'; head -c 5242880 /dev/zero | tr '\0' x; printf '"}\n'
printf '%s\n' '{"type":"result","subtype":"success","total_cost_usd":0.25}'
echo 'stderr line two token=sekrit99' >&2
exit 3
`

// The run's stdout lands in <id>.stream.jsonl, masked and otherwise byte for
// byte, including a 5 MiB line. Its stderr lands in the run log and the
// durable log, masked, and never in the stream. The exit code is the run's.
func TestPipeRunStreamFile(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", streamScript)
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())

	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	rec := waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeFailed))[0]
	if rec.ExitCode == nil || *rec.ExitCode != 3 {
		t.Fatalf("exit code = %v, want 3", rec.ExitCode)
	}

	streamPath := StreamPathFor(m.RunLogPath("oneshot", 1))
	st, err := os.Stat(streamPath)
	if err != nil {
		t.Fatalf("no stream file: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("stream file mode %v, want 0600", st.Mode().Perm())
	}
	want := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -H \"Authorization: token [REDACTED]\" https://x"}}]}}`,
		`{"type":"user","content":"` + strings.Repeat("x", 5<<20) + `"}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.25}`,
	}, "\n") + "\n"
	got := readText(t, streamPath)
	if got != want {
		t.Errorf("stream file differs from the masked stream: %d bytes, want %d; head %q", len(got), len(want), got[:min(len(got), 300)])
	}
	for i, ln := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !json.Valid([]byte(ln)) {
			t.Errorf("stream line %d does not parse", i+1)
		}
	}

	runLog := readText(t, m.RunLogPath("oneshot", 1))
	durable := readText(t, filepath.Join(e.logs, "oneshot.log"))
	for name, text := range map[string]string{"run log": runLog, "durable log": durable} {
		for _, w := range []string{"warning: stderr line one", "stderr line two token=[REDACTED]"} {
			if !strings.Contains(text, w) {
				t.Errorf("%s lacks stderr line %q:\n%s", name, w, text)
			}
		}
		for _, secret := range []string{"sekrit99", "abc123"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s carries %q", name, secret)
			}
		}
		if strings.Contains(text, `"type":"system"`) {
			t.Errorf("%s carries stdout; it belongs in the stream file only", name)
		}
	}
	if strings.Contains(got, "warning: stderr") {
		t.Error("stderr reached the stream file")
	}
	if strings.Index(runLog, "stderr line two") > strings.LastIndex(runLog, "run finished") {
		t.Errorf("the run log's finish line is not last:\n%s", runLog)
	}
}

// A line longer than the cap is read to its end, not kept, and leaves a JSON
// marker; the stream goes on intact after it.
func TestPipeRunDropsOverlongLine(t *testing.T) {
	orig := maxPipeLine
	maxPipeLine = 1024
	t.Cleanup(func() { maxPipeLine = orig })
	e := newRunsEnv(t)
	fakeAgent(t, "claude", `printf '{"type":"user","content":"'; head -c 5000 /dev/zero | tr '\0' y; printf '"}\n'
printf '%s\n' '{"type":"result"}'
`)
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())
	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeSuccess))

	got := readText(t, StreamPathFor(m.RunLogPath("oneshot", 1)))
	want := `{"harness":"line_dropped","stream":"stdout","bytes":5028,"limit":1024}` + "\n" + `{"type":"result"}` + "\n"
	if got != want {
		t.Errorf("stream = %q, want %q", got, want)
	}
}

// A stop reaches the pipe run's whole process group, as it does a PTY run's:
// a child that ignores SIGTERM and SIGHUP is SIGKILLed once the grace runs
// out, and the stop is recorded cancelled.
func TestPipeRunStopKillsProcessGroup(t *testing.T) {
	e := newRunsEnv(t)
	child := filepath.Join(e.dir, "child.pid")
	fakeAgent(t, "claude", fmt.Sprintf(`sh -c 'trap "" HUP TERM; echo $$ > %s; while :; do sleep 0.05; done' &
echo '{"type":"system"}'
while :; do sleep 0.05; done
`, child))
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())
	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	pid := readPidFile(t, child)
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil && processLive(pid) {
			_ = p.Kill()
		}
	})

	m.Stop("oneshot")
	waitRuns(t, m, "oneshot", "run cancelled", outcomesAre(OutcomeCancelled))
	waitFor(t, 3*time.Second, "the child that ignores SIGTERM is gone", func() bool { return !processLive(pid) })
}

// When the leader exits on its own, what is left of its group gets the SIGHUP
// a terminal would have sent, so a child holding the pipes open dies with the
// run instead of outliving it.
func TestPipeRunNaturalExitHangsUpTheGroup(t *testing.T) {
	e := newRunsEnv(t)
	child := filepath.Join(e.dir, "child.pid")
	fakeAgent(t, "claude", fmt.Sprintf(`sh -c 'trap "" TERM; echo $$ > %s; while :; do sleep 0.05; done' &
while [ ! -s %s ]; do sleep 0.01; done
echo '{"type":"result"}'
`, child, child))
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())
	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	pid := readPidFile(t, child)
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil && processLive(pid) {
			_ = p.Kill()
		}
	})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeSuccess))
	waitFor(t, 3*time.Second, "the child is hung up", func() bool { return !processLive(pid) })
}

// A prompt harness with no triggers has no per-run files, so its stdout goes
// to its durable log, masked, beside its stderr.
func TestPipeRunWithoutARunLogWritesTheDurableLog(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", `echo '{"type":"system","command":"GITEA_TOKEN=abc123 tea"}'
echo 'from stderr' >&2
`)
	h := pipeSweep("adhoc")
	h.Schedule = ""
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	m.Start("adhoc")
	path := filepath.Join(e.logs, "adhoc.log")
	waitFor(t, 5*time.Second, "stdout and stderr in the durable log", func() bool {
		b, _ := os.ReadFile(path)
		return strings.Contains(string(b), `{"type":"system","command":"GITEA_TOKEN=[REDACTED] tea"}`) &&
			strings.Contains(string(b), "from stderr")
	})
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "abc123") {
		t.Error("the durable log carries the credential")
	}
	if matches, _ := filepath.Glob(filepath.Join(e.jobs, "adhoc", "*")); len(matches) != 0 {
		t.Errorf("an untriggered harness wrote per-run files: %v", matches)
	}
}

// keep_runs prunes a pipe run's stream file with its log.
func TestPipeRunKeepRunsPrunesStream(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", `echo '{"type":"result"}'`+"\n")
	h := pipeSweep("oneshot")
	h.KeepRuns = 2
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	for i := 1; i <= 4; i++ {
		m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
		want := make([]RunOutcome, i)
		for j := range want {
			want[j] = OutcomeSuccess
		}
		waitRuns(t, m, "oneshot", "run "+strconv.Itoa(i)+" ends", outcomesAre(want...))
	}
	// Pruning happens as a run opens: run 4 opening kept runs 3 and 4.
	for id := 1; id <= 4; id++ {
		_, err := os.Stat(StreamPathFor(m.RunLogPath("oneshot", id)))
		if kept := id >= 3; kept != (err == nil) {
			t.Errorf("run %d stream file: stat err %v, want kept=%v", id, err, kept)
		}
	}
}

// The attach tee (ExtraOut) receives stdout and stderr as the CRLF-terminated
// lines a terminal would show, masked, and never a bare LF.
func TestPipeRunTeesLinesToExtraOut(t *testing.T) {
	dir := t.TempDir()
	fakeAgent(t, "claude", `echo '{"type":"system","command":"GITEA_TOKEN=abc123 tea"}'
echo 'stderr says hi' >&2
echo '{"type":"result"}'
`)
	var tee safeBuffer
	m := NewManager(sweepCfg(pipeSweep("oneshot")), ManagerOptions{
		Policy:      fastPolicy(),
		StatePath:   filepath.Join(dir, "state.json"),
		LogDir:      filepath.Join(dir, "logs"),
		ExtraOutFor: func(string) io.Writer { return &tee },
	})
	t.Cleanup(m.Close)
	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeSuccess))

	got := tee.String()
	for _, w := range []string{"{\"type\":\"system\",\"command\":\"GITEA_TOKEN=[REDACTED] tea\"}\r\n", "stderr says hi\r\n", "{\"type\":\"result\"}\r\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("tee lacks %q:\n%q", w, got)
		}
	}
	if strings.Contains(got, "abc123") {
		t.Error("the tee carries the credential unmasked")
	}
	if n := strings.Count(got, "\n"); n != strings.Count(got, "\r\n") {
		t.Errorf("tee has %d LFs but only %d CRLFs: %q", n, strings.Count(got, "\r\n"), got)
	}
}

// The property the pipe path exists for: a pipe run constructs no terminal
// emulator. Counted at the constructor. The PTY control shows the counter
// sees an emulator when one is built.
func TestPipeRunBuildsNoEmulator(t *testing.T) {
	var built atomic.Int64
	orig := newEmulator
	newEmulator = func(w, h int) *vt.Emulator {
		built.Add(1)
		return orig(w, h)
	}
	t.Cleanup(func() { newEmulator = orig })

	e := newRunsEnv(t)
	fakeAgent(t, "claude", `echo '{"type":"result"}'`+"\n")
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot"), sweep("control", "echo hi")), fastPolicy())

	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "oneshot", "pipe run ends", outcomesAre(OutcomeSuccess))
	if n := built.Load(); n != 0 {
		t.Fatalf("a pipe run built %d emulators, want 0", n)
	}
	m.StartRun("control", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "control", "PTY run ends", outcomesAre(OutcomeSuccess))
	if built.Load() == 0 {
		t.Fatal("the PTY control built no emulator, so the counter cannot see one")
	}
}

// Retained heap stays flat across pipe runs: nothing a run printed outlives
// it. Each run prints about a megabyte; a leak of a fraction of that per run
// shows over the runs measured. HARNESS_HEAP_RUNS raises the count for a
// manual measurement.
func TestPipeRunRetainedHeapIsFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns many runs")
	}
	runs := 20
	if n, err := strconv.Atoi(os.Getenv("HARNESS_HEAP_RUNS")); err == nil && n > 0 {
		runs = n
	}
	e := newRunsEnv(t)
	// Eight lines of 128 KiB, each a tool result of short text lines joined
	// by JSON-escaped newlines: the shape (and the masking cost) of a real
	// file read.
	fakeAgent(t, "claude", `i=0; while [ $i -lt 8 ]; do printf '{"type":"user","content":"'; yes 'a line of the file being read, as a tool result' | head -n 2600 | sed 's/$/\\n/' | tr -d '\n'; printf '"}\n'; i=$((i+1)); done
`)
	h := pipeSweep("oneshot")
	h.KeepRuns = 3
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	var base uint64
	for i := 1; i <= runs; i++ {
		m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
		waitRuns(t, m, "oneshot", "run "+strconv.Itoa(i)+" ends", func(rs []RunRecord) bool {
			return len(rs) == i && rs[i-1].Outcome == OutcomeSuccess
		})
		if i == 5 {
			base = heap()
		}
	}
	end := heap()
	t.Logf("retained heap after run 5: %d KiB; after run %d: %d KiB (%+d KiB)", base>>10, runs, end>>10, (int64(end)-int64(base))>>10)
	if end > base+4<<20 {
		t.Errorf("retained heap grew %d KiB over %d runs", (end-base)>>10, runs-5)
	}
	// And the output did land: the last run's stream holds its 8 lines.
	if b, err := os.ReadFile(StreamPathFor(m.RunLogPath("oneshot", runs))); err != nil || bytes.Count(b, []byte("\n")) != 8 {
		t.Fatalf("run %d stream: %v, %d lines", runs, err, bytes.Count(b, []byte("\n")))
	}
}
