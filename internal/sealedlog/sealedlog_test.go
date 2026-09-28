package sealedlog

// Sealed Log Tests
//
// What is pinned here is the contract every reader relies on: a sealed file
// round-trips byte for byte, a reader gets the same bytes from either form,
// the plain form wins when both exist, and no step of a compression — a crash
// between steps, a failed sync, a shutdown, a file that turns out to be still
// growing — ever leaves a log with neither form on disk.
//
// None of these wait on a timer. The worker is observed through Wait, and the
// sync hooks are what make the failure paths deterministic.
//
// Governing: ADR-0007 (as amended for sealed compression); SPEC-0003 REQ
// "Durable Log Rotation And Compression"; SPEC-0008 REQ "Per-Run Logs".

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestMain(m *testing.M) {
	SkipSyncForTesting()
	os.Exit(m.Run())
}

// fixture is a sanitized agent log of roughly n bytes, shaped like the ones
// the daemon writes: charmbracelet lifecycle lines, tool-call narration, file
// paths, diff and test output, and the incompressible fragments real output
// carries (commit SHAs, request ids). Seeded, so every run is the same file.
func fixture(n int) []byte {
	r := rand.New(rand.NewPCG(18, 2026))
	verbs := []string{"Read", "Edit", "Bash", "Grep", "Write", "Glob"}
	paths := []string{"internal/supervisor/runs.go", "internal/daemon/jobs.go", "cmd/harness/logs.go", "internal/ledger/index.go", "docs/usage/cli.md"}
	prose := []string{
		"I'll check how the run log is opened before changing the prune.",
		"The test fails because the fixture is built before the manager starts.",
		"Let me look at the reader that serves `harness logs --run`.",
		"That matches the spec; moving on to the rotation path.",
		"ok  \tgithub.com/stump-wtf/harness/internal/supervisor\t4.218s",
		"--- PASS: TestPruneRunLogsKeepsNewest (0.02s)",
		"+\tif id, ok := runArtifactID(e.Name()); ok && drop[id] {",
		"-\tif id, ok := runLogID(e.Name()); ok && drop[id] {",
	}
	var b bytes.Buffer
	at := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	for b.Len() < n {
		at = at.Add(time.Duration(r.IntN(4000)) * time.Millisecond)
		switch r.IntN(10) {
		case 0:
			fmt.Fprintf(&b, "%s INFO state changed from=running to=%s\n", at.Format("2006/01/02 15:04:05"), []string{"stopping", "failed", "running"}[r.IntN(3)])
		case 1, 2:
			fmt.Fprintf(&b, "● %s(%s)\n  ⎿  %d lines\n", verbs[r.IntN(len(verbs))], paths[r.IntN(len(paths))], r.IntN(900)+1)
		case 3:
			fmt.Fprintf(&b, "commit %016x%016x%08x request_id=req_%012x\n", r.Uint64(), r.Uint64(), r.Uint32(), r.Uint64()&0xffffffffffff)
		default:
			b.WriteString(prose[r.IntN(len(prose))])
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// temps lists compression temps left in dir.
func temps(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if _, ok := TempOf(e.Name()); ok {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestCompressFileRoundTrip is the core promise: the plain file is replaced
// by a compressed one that decodes to exactly the same bytes, keeps the
// plain file's permissions and mtime, and is meaningfully smaller.
func TestCompressFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "7.log")
	data := fixture(4 << 20)
	writeFile(t, path, data, 0o600)
	mtime := time.Date(2026, 9, 27, 12, 0, 0, 0, time.Local)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	if err := CompressFile(path); err != nil {
		t.Fatal(err)
	}
	if exists(path) {
		t.Fatalf("plain %s still exists after compression", path)
	}
	info, err := os.Stat(Compressed(path))
	if err != nil {
		t.Fatalf("compressed file missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("compressed mode = %o, want 0600 (the plain file's)", got)
	}
	if !info.ModTime().Equal(mtime) {
		t.Errorf("compressed mtime = %v, want the plain file's %v", info.ModTime(), mtime)
	}
	if got := readAll(t, path); !bytes.Equal(got, data) {
		t.Fatalf("round trip differs: got %d bytes, want %d", len(got), len(data))
	}
	ratio := float64(len(data)) / float64(info.Size())
	t.Logf("fixture %d bytes -> %d bytes (%.2fx)", len(data), info.Size(), ratio)
	if ratio < 3 {
		t.Errorf("ratio %.2fx on a log-shaped fixture; want at least 3x", ratio)
	}
	if left := temps(t, dir); len(left) > 0 {
		t.Errorf("temps left behind: %v", left)
	}
}

// TestReadersSeeEitherForm reads one log through every reader entry point in
// its plain form, its compressed form, and with both present.
func TestReadersSeeEitherForm(t *testing.T) {
	data := fixture(200 << 10)
	for _, form := range []string{"plain", "compressed", "both"} {
		t.Run(form, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "3.log")
			writeFile(t, path, data, 0o600)
			switch form {
			case "compressed":
				if err := CompressFile(path); err != nil {
					t.Fatal(err)
				}
			case "both":
				if err := CompressFile(path); err != nil {
					t.Fatal(err)
				}
				writeFile(t, path, data, 0o600) // the crash between rename and remove
			}
			if got := readAll(t, path); !bytes.Equal(got, data) {
				t.Errorf("Open: got %d bytes, want %d", len(got), len(data))
			}
			if Missing(path) {
				t.Error("Missing reported a present log")
			}
			if _, err := Stat(path); err != nil {
				t.Errorf("Stat: %v", err)
			}
			tail, cut, err := Tail(path, 1000)
			if err != nil || !cut || !bytes.Equal(tail, data[len(data)-1000:]) {
				t.Errorf("Tail = %d bytes, cut %v, err %v; want the last 1000 bytes, cut", len(tail), cut, err)
			}
			lines, err := TailLines(path, 5)
			if err != nil || !bytes.Equal(lines, referenceTailLines(data, 5)) {
				t.Errorf("TailLines = %q, %v; want %q", lines, err, referenceTailLines(data, 5))
			}
		})
	}
}

// TestOpenPrefersPlain pins the tie-break: with both forms present the plain
// file is what a reader gets.
func TestOpenPrefersPlain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web-20260928T090000.000.log")
	writeFile(t, path, []byte("compressed copy\n"), 0o644)
	if err := CompressFile(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, []byte("plain copy\n"), 0o644)
	if got := string(readAll(t, path)); got != "plain copy\n" {
		t.Errorf("Open read %q, want the plain file", got)
	}
}

func TestMissingLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "9.log")
	if _, err := Open(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(missing) = %v, want fs.ErrNotExist", err)
	}
	if !Missing(path) {
		t.Error("Missing(missing) = false")
	}
	if _, err := TailLines(path, 3); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("TailLines(missing) = %v, want fs.ErrNotExist", err)
	}
	if err := CompressFile(path); err != nil {
		t.Errorf("CompressFile(missing) = %v, want nil (already compressed or pruned)", err)
	}
}

// TestCompressorSealsInBackground drives the worker the way the daemon does:
// Seal returns at once, Wait observes completion, a duplicate or a missing
// path is harmless, and a nil Compressor (compression off) does nothing.
func TestCompressorSealsInBackground(t *testing.T) {
	dir := t.TempDir()
	c := NewCompressor()
	t.Cleanup(c.Close)
	var paths []string
	for i := 1; i <= 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%d.log", i))
		writeFile(t, p, fixture(64<<10), 0o600)
		paths = append(paths, p)
	}
	c.Seal(paths...)
	c.Seal(paths[0], filepath.Join(dir, "gone.log"), "")
	c.Wait()
	for _, p := range paths {
		if exists(p) || !exists(Compressed(p)) {
			t.Errorf("%s: plain %v, compressed %v; want only compressed", p, exists(p), exists(Compressed(p)))
		}
	}

	var off *Compressor
	p := filepath.Join(dir, "kept.log")
	writeFile(t, p, []byte("x\n"), 0o600)
	off.Seal(p)
	off.Wait()
	off.Close()
	if !exists(p) || exists(Compressed(p)) {
		t.Error("a nil Compressor compressed a file")
	}
}

// TestCompressBothPresent covers the boot sweep's leftover: a crash after the
// rename and before the plain file's removal. A compressed file that decodes
// to the plain one is kept and the plain removed; one that does not is
// rewritten, never trusted.
func TestCompressBothPresent(t *testing.T) {
	data := fixture(100 << 10)
	t.Run("whole", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "4.log")
		writeFile(t, path, data, 0o600)
		if err := CompressFile(path); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, data, 0o600)
		if err := CompressFile(path); err != nil {
			t.Fatal(err)
		}
		if exists(path) || !bytes.Equal(readAll(t, path), data) {
			t.Error("the plain file should be gone and the compressed one intact")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "4.log")
		writeFile(t, path, data, 0o600)
		writeFile(t, Compressed(path), []byte("not zstd"), 0o600)
		if err := CompressFile(path); err != nil {
			t.Fatal(err)
		}
		if exists(path) || !bytes.Equal(readAll(t, path), data) {
			t.Error("a corrupt compressed file must be rewritten from the plain one")
		}
	})
}

// TestCompressRemovesStaleTemp: a crash mid-encode leaves a temp; the next
// compression of the same file clears it.
func TestCompressRemovesStaleTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "5.log")
	writeFile(t, path, fixture(10<<10), 0o600)
	stale := filepath.Join(dir, ".5.log"+tempInfix+"123456")
	writeFile(t, stale, []byte("half a frame"), 0o600)
	other := filepath.Join(dir, ".15.log"+tempInfix+"999")
	writeFile(t, other, []byte("another file's temp"), 0o600)
	if err := CompressFile(path); err != nil {
		t.Fatal(err)
	}
	if exists(stale) {
		t.Error("stale temp for 5.log survived")
	}
	if !exists(other) {
		t.Error("a temp belonging to 15.log was removed while compressing 5.log")
	}
}

// TestCompressLeavesGrowingFile: a file written to while it is compressed was
// not sealed. It is left plain with every byte, and no compressed file
// appears.
func TestCompressLeavesGrowingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "6.log")
	writeFile(t, path, []byte("first\n"), 0o600)
	prev := syncFile
	t.Cleanup(func() { syncFile = prev })
	syncFile = func(*os.File) error {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = f.WriteString("late\n")
		return err
	}
	err := CompressFile(path)
	if !errors.Is(err, errGrowing) {
		t.Fatalf("CompressFile = %v, want errGrowing", err)
	}
	if exists(Compressed(path)) {
		t.Error("a compressed file appeared for a growing log")
	}
	if got, _ := os.ReadFile(path); string(got) != "first\nlate\n" {
		t.Errorf("plain file = %q, want every byte written", got)
	}
	if left := temps(t, dir); len(left) > 0 {
		t.Errorf("temps left behind: %v", left)
	}
}

// TestCompressDirSyncFailureKeepsPlain: if the directory sync fails the new
// name may not be durable, so the plain file stays; a later pass finishes.
func TestCompressDirSyncFailureKeepsPlain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "8.log")
	data := fixture(20 << 10)
	writeFile(t, path, data, 0o600)
	prev := syncDir
	t.Cleanup(func() { syncDir = prev })
	syncDir = func(string) error { return syscall.EIO }
	if err := CompressFile(path); err == nil {
		t.Fatal("CompressFile succeeded through a failed directory sync")
	}
	if !exists(path) || !exists(Compressed(path)) {
		t.Fatalf("want both forms kept; plain %v, compressed %v", exists(path), exists(Compressed(path)))
	}
	if !bytes.Equal(readAll(t, path), data) {
		t.Error("reader lost bytes between the two forms")
	}
	syncDir = func(string) error { return nil }
	if err := CompressFile(path); err != nil {
		t.Fatal(err)
	}
	if exists(path) {
		t.Error("the retry left the plain file")
	}
}

// TestCompressorCloseNeverLosesALog: Close drops the queue and abandons the
// file in flight, but every log still exists in one form or the other and no
// temp is left.
func TestCompressorCloseNeverLosesALog(t *testing.T) {
	dir := t.TempDir()
	c := NewCompressor()
	var paths []string
	for i := 1; i <= 20; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%d.log", i))
		writeFile(t, p, fixture(256<<10), 0o600)
		paths = append(paths, p)
	}
	c.Seal(paths...)
	c.Close()
	c.Close() // idempotent
	c.Seal(paths[0])
	c.Wait() // returns: closed
	for _, p := range paths {
		if Missing(p) {
			t.Errorf("%s has neither form after Close", p)
		}
	}
	if left := temps(t, dir); len(left) > 0 {
		t.Errorf("temps left behind: %v", left)
	}
}

// TestDecoderRefusesHugeWindow: a hand-placed file whose window would make a
// reader allocate far more than ours is refused, not honoured.
func TestDecoderRefusesHugeWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "1.log")
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithWindowSize(64<<20), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = enc.Write(fixture(1 << 20)) // over one block, so the header carries the window
	_ = enc.Close()
	writeFile(t, Compressed(path), buf.Bytes(), 0o600)
	r, err := Open(path)
	if err == nil {
		_, err = io.ReadAll(r)
		_ = r.Close()
	}
	if err == nil {
		t.Fatal("a 64 MiB-window file decoded; want it refused")
	}
}

func TestTempOfAndPlain(t *testing.T) {
	for _, tt := range []struct {
		name, orig string
		ok         bool
	}{
		{".3.log.zst-tmp-12345", "3.log", true},
		{".web-20260928T090000.000.log.zst-tmp-9", "web-20260928T090000.000.log", true},
		{"3.log.zst-tmp-12345", "", false},
		{".3.log.zst", "", false},
		{"3.log", "", false},
	} {
		orig, ok := TempOf(tt.name)
		if orig != tt.orig || ok != tt.ok {
			t.Errorf("TempOf(%q) = %q, %v; want %q, %v", tt.name, orig, ok, tt.orig, tt.ok)
		}
	}
	if p, ok := Plain("jobs/x/3.log.zst"); p != "jobs/x/3.log" || !ok {
		t.Errorf("Plain = %q, %v", p, ok)
	}
	if p, ok := Plain("jobs/x/3.log"); p != "jobs/x/3.log" || ok {
		t.Errorf("Plain(uncompressed) = %q, %v", p, ok)
	}
}

// referenceTailLines is the daemon's tailLines (internal/daemon/logs.go),
// which reads a whole plain file: TailLines must agree with it exactly.
func referenceTailLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	end := len(data)
	search := data
	if search[end-1] == '\n' {
		search = search[:end-1]
	}
	count := 0
	for i := len(search) - 1; i >= 0; i-- {
		if search[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}

func TestTailLinesMatchesReference(t *testing.T) {
	inputs := []string{"", "a", "a\n", "a\nb", "a\nb\n", "a\nb\n\n", "\n\n\n", "x\ny\nz\nw\n", strings.Repeat("line\n", 50)}
	for _, in := range inputs {
		for _, n := range []int{0, 1, 2, 3, 10} {
			for _, compressed := range []bool{false, true} {
				dir := t.TempDir()
				path := filepath.Join(dir, "1.log")
				writeFile(t, path, []byte(in), 0o600)
				if compressed {
					if err := CompressFile(path); err != nil {
						t.Fatal(err)
					}
				}
				got, err := TailLines(path, n)
				if err != nil {
					t.Fatal(err)
				}
				if want := referenceTailLines([]byte(in), n); !bytes.Equal(got, want) {
					t.Errorf("TailLines(%q, %d, compressed=%v) = %q, want %q", in, n, compressed, got, want)
				}
			}
		}
	}
}
