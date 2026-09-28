package attach

// Line mode across the real seam: attach Registry, supervisor Manager, and a
// fake `claude` first on PATH, spawned exactly as a real stream-json one-shot
// is. A PTY control (a fake `crush` printing the same lines) runs beside it,
// so every "none" below is measured by a probe that has seen one.
//
// Governing: ADR-0033 "Structured one-shots run on pipes"; SPEC-0017 REQ-18;
// SPEC-0002 REQ "Attach Session".

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// e2eScript prints three stream-json lines and a stderr line.
const e2eScript = `echo '{"type":"system","subtype":"init"}'
echo 'warning: from stderr' >&2
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}'
echo '{"type":"result","subtype":"success"}'
`

// fakeAgents puts executables named after each key, running its sh script,
// first on PATH.
func fakeAgents(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for exe, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, exe), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// oneShot is a scheduled prompt one-shot of kind adapter.
func oneShot(name, adapter, workdir string) core.Harness {
	return core.Harness{
		Name: name, Adapter: adapter, Prompt: "triage the queue", Workdir: workdir,
		Backend: core.BackendNative, Restart: core.RestartNo, Schedule: "0 3 * * *",
		OnOverlap: core.OverlapSkip, KeepRuns: 3,
	}
}

// wiredManager builds a Manager wired to r the way the daemon wires it.
func wiredManager(t *testing.T, r *Registry, hs ...core.Harness) *supervisor.Manager {
	t.Helper()
	tmp := t.TempDir()
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}
	for _, h := range hs {
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	m := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		Policy:       supervisor.Policy{StopGrace: 200 * time.Millisecond},
		StatePath:    filepath.Join(tmp, "state.json"),
		LogDir:       filepath.Join(tmp, "logs"),
		ExtraOutFor:  r.WriterFor,
		DropExtraOut: r.Remove,
		SizeFor:      r.SizeFor,
	})
	r.SetController(m)
	t.Cleanup(m.Close)
	return m
}

// runOnce fires name and waits for its n-th run to finish.
func runOnce(t *testing.T, m *supervisor.Manager, name string, n int) supervisor.RunRecord {
	t.Helper()
	m.StartRun(name, supervisor.RunRequest{Trigger: supervisor.TriggerManual})
	var rec supervisor.RunRecord
	waitForCond(t, name+" run "+strconv.Itoa(n)+" ends", func() bool {
		rs := m.Runs(name)
		if len(rs) != n || rs[n-1].Outcome == supervisor.OutcomeRunning {
			return false
		}
		rec = rs[n-1]
		return true
	})
	return rec
}

// A stream-json one-shot's attach session gets its output as CRLF lines, and
// neither its supervisor nor this package ever builds an emulator for it. The
// PTY control, built with the Manager, shows the counter sees one.
func TestPipeRunAttachesThroughTheLineMux(t *testing.T) {
	built := countEmulators(t)
	fakeAgents(t, map[string]string{"claude": e2eScript, "crush": e2eScript})
	r := NewRegistry(0)
	dir := t.TempDir()
	m := wiredManager(t, r, oneShot("oneshot", "claude-code", dir), oneShot("control", "crush", dir))
	if n := built.Load(); n != 1 {
		t.Fatalf("building the Manager built %d emulators, want 1 (the PTY control's Mux)", n)
	}

	// The daemon's attach path: the definition decides before the first run.
	var rec recorder
	target := r.AttachTarget("oneshot", true)
	if _, ok := target.(*LineMux); !ok {
		t.Fatalf("a never-run stream-json one-shot attached to %T", target)
	}
	s := target.Attach(1, protocol.AttachRO, 120, 40, rec.write)
	defer s.Detach()

	if got := runOnce(t, m, "oneshot", 1); got.Outcome != supervisor.OutcomeSuccess {
		t.Fatalf("pipe run = %+v", got)
	}
	for _, want := range []string{
		"{\"type\":\"system\",\"subtype\":\"init\"}\r\n",
		"warning: from stderr\r\n",
		"{\"type\":\"result\",\"subtype\":\"success\"}\r\n",
	} {
		waitForCond(t, "the session receives "+want, func() bool { return strings.Contains(rec.String(), want) })
	}
	if n := built.Load(); n != 1 {
		t.Fatalf("a pipe run and its attach built %d emulators in all, want only the control's 1", n)
	}
	if snap, ok := r.SnapshotFor("oneshot"); !ok || len(snap.Sessions) != 1 || snap.Cols != 0 {
		t.Errorf("describe = %+v, %v; want the session and no viewport", snap, ok)
	}

	// The control's PTY run writes its Mux, whose emulator already exists.
	runOnce(t, m, "control", 1)
	if _, ok := r.AttachTarget("control", true).(*Mux); !ok {
		t.Error("after a PTY run, the control does not attach to its terminal mux")
	}
}

// Retained heap stays flat across pipe runs with a session attached, the whole
// daemon data plane included: nothing a run prints outlives it but the
// byte-bounded recent-lines ring. HARNESS_HEAP_PTY=1 also measures the PTY
// control printing the same output, for comparison; HARNESS_HEAP_RUNS raises
// the count.
func TestPipeRunDataPlaneHeapIsFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns many runs")
	}
	runs := 20
	if n, err := strconv.Atoi(os.Getenv("HARNESS_HEAP_RUNS")); err == nil && n > 0 {
		runs = n
	}
	// Eight 128 KiB tool results a run, as short text lines joined by
	// JSON-escaped newlines, the shape of a real file read.
	body := `i=0; while [ $i -lt 8 ]; do printf '{"type":"user","content":"'; yes 'a line of the file being read, as a tool result' | head -n 2600 | sed 's/$/\\n/' | tr -d '\n'; printf '"}\n'; i=$((i+1)); done
`
	fakeAgents(t, map[string]string{"claude": body, "crush": body})
	r := NewRegistry(0)
	dir := t.TempDir()
	m := wiredManager(t, r, oneShot("oneshot", "claude-code", dir), oneShot("control", "crush", dir))
	var rec recorder
	s := r.AttachTarget("oneshot", true).Attach(1, protocol.AttachRO, 120, 40, func(p []byte) error {
		rec.mu.Lock()
		rec.buf.Reset() // a client that reads and discards
		rec.mu.Unlock()
		return nil
	})
	defer s.Detach()

	heap := func() uint64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	measure := func(name string) (base, end uint64) {
		for i := 1; i <= runs; i++ {
			runOnce(t, m, name, i)
			if i == 5 {
				base = heap()
			}
		}
		return base, heap()
	}
	pb, pe := measure("oneshot")
	t.Logf("pipe runs: retained heap %d KiB after run 5, %d KiB after run %d (%+d KiB)", pb>>10, pe>>10, runs, (int64(pe)-int64(pb))>>10)
	if os.Getenv("HARNESS_HEAP_PTY") != "" {
		// Opt-in: before the emulator fixes this retains gigabytes, more
		// than a CI runner should be asked for.
		cb, ce := measure("control")
		t.Logf("PTY control, same output: %d KiB after run 5, %d KiB after run %d (%+d KiB)", cb>>10, ce>>10, runs, (int64(ce)-int64(cb))>>10)
	}
	if pe > pb+4<<20 {
		t.Errorf("retained heap grew %d KiB over %d pipe runs", (pe-pb)>>10, runs-5)
	}
}
