package supervisor

// Sealed Log Compression Tests
//
// A sealed log — a rotated backup of a durable log, the log and raw stream of
// a closed run — is compressed to <file>.zst in the background. These drive
// the real paths that seal files (rotation, a run closing, the boot sweep,
// the prune) and then read the result back through the same readers the
// daemon serves from, in each form.
//
// Nothing here races a timer against the compressor: the Manager's sealer is
// drained with Wait, which returns once every queued file has been handled,
// and sealedlog.SkipSyncForTesting (TestMain) keeps the runner's slow fsync
// out of it.
//
// Governing: ADR-0007 (as amended for sealed compression); SPEC-0003 REQ
// "Durable Log Rotation And Compression"; SPEC-0008 REQ "Per-Run Logs";
// GitHub https://github.com/stump-wtf/harness/issues/18.

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/sealedlog"
)

// compressedManager is runsEnv.manager with compression on, as the daemon
// runs it.
func (e runsEnv) compressedManager(t *testing.T, cfg *core.Config) (*Manager, func()) {
	t.Helper()
	m := NewManager(cfg, ManagerOptions{Policy: fastPolicy(), StatePath: e.state, LogDir: e.logs, CompressLogs: true})
	closeOnce := sync.OnceFunc(m.Close)
	t.Cleanup(closeOnce)
	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return m, closeOnce
}

func present(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readSealed(t *testing.T, path string) string {
	t.Helper()
	r, err := sealedlog.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustCompress(t *testing.T, path string) {
	t.Helper()
	if err := sealedlog.CompressFile(path); err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// TestRotationCompressesBackups: every rotated backup ends up compressed, the
// active file never is, and reading the backups oldest first and then the
// active file gives back every byte written, in order.
func TestRotationCompressesBackups(t *testing.T) {
	dir := t.TempDir()
	sealer := sealedlog.NewCompressor()
	t.Cleanup(sealer.Close)
	rl, err := newRotatingLog("demo", LogConfig{Dir: dir, MaxBytes: 64, MaxBackups: 100, Sealer: sealer})
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := range 20 {
		line := fmt.Sprintf("line %02d of the demo harness log\n", i)
		want.WriteString(line)
		if _, err := rl.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}
	sealer.Wait()

	backups := listBackups(dir, "demo")
	if len(backups) < 5 {
		t.Fatalf("want several rotations, got %d backups: %v", len(backups), dirNames(t, dir))
	}
	var got strings.Builder
	for _, b := range backups {
		if b.plain || present(b.path) || !present(sealedlog.Compressed(b.path)) {
			t.Errorf("backup %s is not compressed (plain %v)", b.path, b.plain)
		}
		got.WriteString(readSealed(t, b.path))
	}
	active := filepath.Join(dir, "demo.log")
	if present(sealedlog.Compressed(active)) {
		t.Error("the active log was compressed")
	}
	got.WriteString(readText(t, active))
	if got.String() != want.String() {
		t.Errorf("backups + active differ from what was written:\ngot  %q\nwant %q", got.String(), want.String())
	}
}

// TestRotationPruneCountsCompressedBackups: MaxBackups counts a backup once
// whichever form it is in, and pruning "web" never touches "web-api", whose
// name "web" is a prefix of — in either form.
func TestRotationPruneCountsCompressedBackups(t *testing.T) {
	dir := t.TempDir()
	sibActive := filepath.Join(dir, "web-api.log")
	sibPlain := filepath.Join(dir, "web-api-20260101T000000.000.log")
	sibZst := filepath.Join(dir, "web-api-20260102T000000.000.log")
	mustWrite(t, sibActive, "sibling active\n")
	mustWrite(t, sibPlain, "sibling plain backup\n")
	mustWrite(t, sibZst, "sibling compressed backup\n")
	mustCompress(t, sibZst)
	// An old backup of "web" itself, left in both forms by a crash.
	own := filepath.Join(dir, "web-20250101T000000.000.log")
	mustWrite(t, own, "old web backup\n")
	mustCompress(t, own)
	mustWrite(t, own, "old web backup\n")

	sealer := sealedlog.NewCompressor()
	t.Cleanup(sealer.Close)
	rl, err := newRotatingLog("web", LogConfig{Dir: dir, MaxBytes: 16, MaxBackups: 2, Sealer: sealer})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := rl.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	_ = rl.Close()
	sealer.Wait()

	if got := readText(t, sibActive); got != "sibling active\n" {
		t.Errorf("sibling active log changed: %q", got)
	}
	if got := readText(t, sibPlain); got != "sibling plain backup\n" {
		t.Errorf("sibling plain backup changed or compressed by web's prune: %q", got)
	}
	if got := readSealed(t, sibZst); got != "sibling compressed backup\n" || present(sibZst) {
		t.Errorf("sibling compressed backup disturbed: %q", got)
	}
	backups := listBackups(dir, "web")
	if len(backups) != 2 {
		t.Fatalf("web keeps %d backups, want MaxBackups=2: %v", len(backups), dirNames(t, dir))
	}
	if !sealedlog.Missing(own) {
		t.Error("the oldest web backup survived the prune in one of its forms")
	}
	for _, b := range backups {
		if b.plain {
			t.Errorf("retained backup %s left plain", b.path)
		}
	}
}

func TestIsBackupOfBothForms(t *testing.T) {
	for _, tt := range []struct {
		name, path string
		want       bool
	}{
		{"web", "/l/web-20260928T090000.000.log", true},
		{"web", "/l/web-20260928T090000.000.log.zst", true},
		{"web", "/l/web-api-20260928T090000.000.log.zst", false},
		{"web", "/l/web-api.log", false},
		{"web", "/l/web.log.zst", false},
		{"web", "/l/web-20260928T090000.000.zst", false},
		{"reduit/agent", "/l/reduit/agent-20260928T090000.000.log.zst", true},
	} {
		if got := isBackupOf(tt.name, tt.path); got != tt.want {
			t.Errorf("isBackupOf(%q, %q) = %v, want %v", tt.name, tt.path, got, tt.want)
		}
	}
}

// TestRemoveLogArtifactsCompressed: tearing a project harness down removes
// its compressed backups and an interrupted compression's temp too, and still
// leaves the sibling alone.
func TestRemoveLogArtifactsCompressed(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "reduit")
	active := filepath.Join(sub, "agent.log")
	plain := filepath.Join(sub, "agent-20260101T000000.000.log")
	zst := filepath.Join(sub, "agent-20260102T000000.000.log")
	mustWrite(t, active, "active\n")
	mustWrite(t, plain, "plain\n")
	mustWrite(t, zst, "compressed\n")
	mustCompress(t, zst)
	mustWrite(t, filepath.Join(sub, ".agent-20260101T000000.000.log.zst-tmp-42"), "half")
	sibling := filepath.Join(sub, "agent-api-20260101T000000.000.log")
	mustWrite(t, sibling, "sibling\n")
	mustCompress(t, sibling)

	removeLogArtifacts(dir, "reduit/agent")

	if got := dirNames(t, sub); !slices.Equal(got, []string{"agent-api-20260101T000000.000.log.zst"}) {
		t.Errorf("after removal %s holds %v, want only the sibling's backup", sub, got)
	}
}

// TestReadLifecycleReadsCompressedBackups: lifecycle lines come back from
// every backup in order whatever its form, and a backup present in both forms
// is read once.
func TestReadLifecycleReadsCompressedBackups(t *testing.T) {
	dir := t.TempDir()
	line := func(clock, to string) string {
		return fmt.Sprintf("2026/09/28 %s INFO state changed from=starting to=%s\n", clock, to)
	}
	b1 := filepath.Join(dir, "sweep-20260928T090000.000.log")
	b2 := filepath.Join(dir, "sweep-20260928T100000.000.log")
	b3 := filepath.Join(dir, "sweep-20260928T110000.000.log")
	mustWrite(t, b1, "output\n"+line("09:00:00", "running"))
	mustWrite(t, b2, line("10:00:00", "failed")+"more output\n")
	mustWrite(t, b3, line("11:00:00", "running"))
	mustWrite(t, filepath.Join(dir, "sweep.log"), line("12:00:00", "stopped"))
	mustCompress(t, b1)
	mustCompress(t, b2)
	mustWrite(t, b2, line("10:00:00", "failed")+"more output\n") // crash leftover: both forms

	entries, err := ReadLifecycle(dir, "sweep", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Time.Format("15:04")+" "+e.Fields["to"])
	}
	want := []string{"09:00 running", "10:00 failed", "11:00 running", "12:00 stopped"}
	if !slices.Equal(got, want) {
		t.Errorf("lifecycle = %v, want %v", got, want)
	}
}

// TestLastOutputLineReadsACompressedRunLog: the notifier quotes a failed
// run's last line the instant it closes, which is the instant its log is
// queued for compression; either form gives the same answer.
func TestLastOutputLineReadsACompressedRunLog(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&b, "working on item %d\n", i)
	}
	b.WriteString("Error: You must be logged in to use Remote Control.\n")
	b.WriteString("2026/09/28 10:00:00 INFO exited code=1\n")
	path := filepath.Join(dir, "3.log")
	mustWrite(t, path, b.String())
	plain := LastOutputLine(path)
	mustCompress(t, path)
	compressed := LastOutputLine(path)
	if want := "Error: You must be logged in to use Remote Control."; plain != want || compressed != want {
		t.Errorf("LastOutputLine plain %q, compressed %q; want %q", plain, compressed, want)
	}
}

// TestRunLogCompressedWhenTheRunCloses drives a real one-shot: while it runs
// its log is plain; once it closes the log is compressed, reads back whole,
// and its record neither loses its log nor reads log_pruned.
func TestRunLogCompressedWhenTheRunCloses(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.compressedManager(t, sweepCfg(sweep("sweep", "echo hello-from-a-sealed-run; exit 0")))
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run finishes", outcomesAre(OutcomeSuccess))
	m.sealer.Wait()

	path := m.RunLogPath("sweep", 1)
	if present(path) || !present(sealedlog.Compressed(path)) {
		t.Fatalf("after close: plain %v, compressed %v; want only the compressed log", present(path), present(sealedlog.Compressed(path)))
	}
	log := readSealed(t, path)
	for _, want := range []string{"run started", "hello-from-a-sealed-run", "run finished"} {
		if !strings.Contains(log, want) {
			t.Errorf("compressed run log missing %q:\n%s", want, log)
		}
	}
	if r, ok := m.Run("sweep", 1); !ok || r.LogPruned {
		t.Errorf("record = %+v, %v; want a record whose log is not pruned", r, ok)
	}
	if rs := m.Runs("sweep"); len(rs) != 1 || rs[0].LogPruned {
		t.Errorf("history = %+v; a compressed log is not a pruned one", rs)
	}
}

// TestRunLogPlainWhileOpen: the log of a run in flight is never touched, by a
// prune or by anything else, however many decisions are made during it.
func TestRunLogPlainWhileOpen(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("sweep", "echo started; sleep 30")
	h.KeepRuns = 1
	m, _ := e.compressedManager(t, sweepCfg(h))
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run in flight", outcomesAre(OutcomeRunning))
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule}) // skipped: overlap
	// A prune on behalf of some other run opening, with keep_runs = 1: run 1
	// is neither dropped nor sealed, because the ledger holds it open.
	m.pruneRunLogs("sweep", 99)
	m.sealer.Wait()
	path := m.RunLogPath("sweep", 1)
	if !present(path) || present(sealedlog.Compressed(path)) {
		t.Errorf("open run's log: plain %v, compressed %v; want plain only", present(path), present(sealedlog.Compressed(path)))
	}
	m.Stop("sweep")
}

// TestKeepRunsCountsCompressedRuns: keep_runs counts a run by its id, in any
// form and with any mix of artifacts, prunes every form of a dropped run, and
// seals a kept run a crash left plain.
func TestKeepRunsCountsCompressedRuns(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("sweep", "echo out; exit 0")
	h.KeepRuns = 3
	jobs := filepath.Join(e.jobs, "sweep")
	// Runs 1-4 from an earlier daemon, every shape a run can leave behind.
	mustWrite(t, filepath.Join(jobs, "1.log"), "one\n")
	mustCompress(t, filepath.Join(jobs, "1.log"))
	mustWrite(t, filepath.Join(jobs, "1.event.json"), "{}")
	mustWrite(t, filepath.Join(jobs, "2.log"), "two\n")
	mustCompress(t, filepath.Join(jobs, "2.log"))
	mustWrite(t, filepath.Join(jobs, "2.stream.jsonl"), "{\"type\":\"result\"}\n")
	mustCompress(t, filepath.Join(jobs, "2.stream.jsonl"))
	mustWrite(t, filepath.Join(jobs, ".2.log.zst-tmp-7"), "half")
	mustWrite(t, filepath.Join(jobs, "3.log"), "three, never compressed\n")
	mustWrite(t, filepath.Join(jobs, "4.log"), "four\n")
	mustCompress(t, filepath.Join(jobs, "4.log"))

	m, _ := e.compressedManager(t, sweepCfg(h))
	m.sealer.Wait() // the boot sweep's work, out of the way
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m, "sweep", "run 5 finishes", func(rs []RunRecord) bool {
		return len(rs) > 0 && rs[len(rs)-1].RunID == 5 && rs[len(rs)-1].Outcome == OutcomeSuccess
	})
	_ = rs
	m.sealer.Wait()

	got := dirNames(t, jobs)
	want := []string{"3.log.zst", "4.log.zst", "5.log.zst"}
	if !slices.Equal(got, want) {
		t.Errorf("jobs dir = %v, want %v (keep_runs=3 by id, every form of 1 and 2 pruned)", got, want)
	}
	if s := readSealed(t, filepath.Join(jobs, "3.log")); s != "three, never compressed\n" {
		t.Errorf("run 3 read back as %q", s)
	}
}

func TestRunLogIDsParseCompressedNames(t *testing.T) {
	for _, tt := range []struct {
		file          string
		logID, artID  int
		isLog, isArt  bool
		sealableFirst bool
	}{
		{"7.log", 7, 7, true, true, true},
		{"7.log.zst", 7, 7, true, true, false},
		{"7.event.json", 0, 7, false, true, false},
		{"7.stream.jsonl", 0, 7, false, true, true},
		{"7.stream.jsonl.zst", 0, 7, false, true, false},
		{".7.log.zst-tmp-1", 0, 0, false, false, false},
		{"x.log.zst", 0, 0, false, false, false},
	} {
		if id, ok := runLogID(tt.file); id != tt.logID || ok != tt.isLog {
			t.Errorf("runLogID(%q) = %d, %v", tt.file, id, ok)
		}
		if id, ok := runArtifactID(tt.file); id != tt.artID || ok != tt.isArt {
			t.Errorf("runArtifactID(%q) = %d, %v", tt.file, id, ok)
		}
		if got := tt.isArt && sealableRunArtifact(tt.file); got != tt.sealableFirst {
			t.Errorf("sealableRunArtifact(%q) = %v, want %v", tt.file, got, tt.sealableFirst)
		}
	}
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "3.log"), "")
	mustWrite(t, filepath.Join(dir, "9.log.zst"), "")
	mustWrite(t, filepath.Join(dir, "12.stream.jsonl.zst"), "")
	if got := highestRunLog(dir); got != 9 {
		t.Errorf("highestRunLog = %d, want 9: a compressed log floors the id, a stream does not", got)
	}
}

// TestBootSealsLeftovers: whatever a dead daemon left uncompressed — a closed
// run's log, a run interrupted by the crash, a crash between rename and
// removal, a stale temp, a plain rotated backup — is sealed at the next boot,
// and every one still reads back.
func TestBootSealsLeftovers(t *testing.T) {
	e := newRunsEnv(t)
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	state := fmt.Sprintf(`{"version":1,"harnesses":{},"runs":{"sweep":{"last_run_id":4,"runs":[`+
		`{"run_id":3,"trigger":"schedule","outcome":"success","started_at":%q,"ended_at":%q,"exit_code":0},`+
		`{"run_id":4,"trigger":"schedule","outcome":"running","started_at":%q}]}}}`,
		started.Format(time.RFC3339), started.Format(time.RFC3339), started.Format(time.RFC3339))
	mustWrite(t, e.state, state)
	jobs := filepath.Join(e.jobs, "sweep")
	mustWrite(t, filepath.Join(jobs, "1.log"), "one\n")
	mustWrite(t, filepath.Join(jobs, "2.log"), "two\n")
	mustCompress(t, filepath.Join(jobs, "2.log"))
	mustWrite(t, filepath.Join(jobs, "2.log"), "two\n") // crash between rename and removal
	mustWrite(t, filepath.Join(jobs, "3.log"), "three\n")
	mustWrite(t, filepath.Join(jobs, "4.log"), "partial output\n") // the run the crash interrupted
	mustWrite(t, filepath.Join(jobs, ".9.log.zst-tmp-3"), "orphan temp")
	backup := filepath.Join(e.logs, "sweep-20260927T000000.000.log")
	mustWrite(t, backup, "an old rotation\n")

	m, _ := e.compressedManager(t, sweepCfg(sweep("sweep", "exit 0")))
	m.sealer.Wait()

	if got := dirNames(t, jobs); !slices.Equal(got, []string{"1.log.zst", "2.log.zst", "3.log.zst", "4.log.zst"}) {
		t.Errorf("jobs dir after boot = %v", got)
	}
	for id, want := range map[int]string{1: "one\n", 2: "two\n", 3: "three\n"} {
		if got := readSealed(t, filepath.Join(jobs, fmt.Sprintf("%d.log", id))); got != want {
			t.Errorf("run %d reads %q, want %q", id, got, want)
		}
	}
	if got := readSealed(t, filepath.Join(jobs, "4.log")); !strings.Contains(got, "partial output") || !strings.Contains(got, "run interrupted") {
		t.Errorf("interrupted run's log lost its reconciliation line:\n%s", got)
	}
	if present(backup) || readSealed(t, backup) != "an old rotation\n" {
		t.Error("the plain rotated backup was not sealed at boot")
	}
}

// TestCompressLogsOff: with compression off (compress_logs = false) nothing
// is compressed — not a closed run, not a rotated backup — and a log an
// earlier daemon compressed is still read, counted and pruned.
func TestCompressLogsOff(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("sweep", "echo plain; exit 0")
	h.KeepRuns = 2
	jobs := filepath.Join(e.jobs, "sweep")
	mustWrite(t, filepath.Join(jobs, "1.log"), "compressed by an earlier daemon\n")
	mustCompress(t, filepath.Join(jobs, "1.log"))
	mustWrite(t, filepath.Join(jobs, "2.log"), "two\n")
	mustCompress(t, filepath.Join(jobs, "2.log"))
	backup := filepath.Join(e.logs, "sweep-20260927T000000.000.log")
	mustWrite(t, backup, "old rotation\n")

	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	if m.sealer != nil {
		t.Fatal("CompressLogs false still built a compressor")
	}
	if r, ok := m.Run("sweep", 2); ok && r.LogPruned {
		t.Errorf("run 2's compressed log reads as pruned with compression off")
	}
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run 3 finishes", func(rs []RunRecord) bool {
		return len(rs) > 0 && rs[len(rs)-1].RunID == 3 && rs[len(rs)-1].Outcome == OutcomeSuccess
	})
	if got := dirNames(t, jobs); !slices.Equal(got, []string{"2.log.zst", "3.log"}) {
		t.Errorf("jobs dir = %v, want [2.log.zst 3.log]: run 3 plain, run 1 pruned by id", got)
	}
	if !present(backup) || present(sealedlog.Compressed(backup)) {
		t.Error("a rotated backup was compressed with compression off")
	}

	rl, err := newRotatingLog("plain", LogConfig{Dir: e.logs, MaxBytes: 8, MaxBackups: 5})
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		_, _ = rl.Write([]byte("0123456789"))
	}
	_ = rl.Close()
	for _, b := range listBackups(e.logs, "plain") {
		if !b.plain || present(sealedlog.Compressed(b.path)) {
			t.Errorf("backup %s compressed with no Sealer", b.path)
		}
	}
}

// TestSealedRunArtifactsIncludeTheStream: a pipe-run one-shot's raw stream
// (ADR-0033) is sealed with its log when the run closes; the event file is
// not, because an operator replays it by path.
func TestSealedRunArtifactsIncludeTheStream(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.compressedManager(t, sweepCfg(sweep("sweep", "exit 0")))
	path := m.RunLogPath("sweep", 1)
	stream := strings.TrimSuffix(path, ".log") + ".stream.jsonl"
	event := strings.TrimSuffix(path, ".log") + ".event.json"
	mustWrite(t, path, "log\n")
	mustWrite(t, stream, "{\"type\":\"assistant\"}\n")
	mustWrite(t, event, "{}")
	m.sealRun("sweep", RunRecord{RunID: 1, Log: path})
	m.sealer.Wait()
	if present(path) || present(stream) {
		t.Errorf("log or stream left plain: log %v, stream %v", present(path), present(stream))
	}
	if !bytes.Equal([]byte(readSealed(t, stream)), []byte("{\"type\":\"assistant\"}\n")) {
		t.Error("the stream did not round-trip")
	}
	if !present(event) || present(sealedlog.Compressed(event)) {
		t.Error("the event file was compressed")
	}
}
