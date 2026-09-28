// Sealed Log Compression
//
// The daemon's logs are append-only text that nobody writes again once they
// are sealed: a rotated backup of a harness's durable log, and the per-run log
// (and raw structured stream) of a run that has closed. Text like that
// compresses several times over, and the retained history belongs on disk
// rather than in memory, so a sealed file is rewritten as `<file>.zst` and the
// plain file removed. The active log, and the log of a run still in flight, are
// never touched: something is still appending to them.
//
// Every reader goes through Open, Stat, Missing or Tail rather than os.Open,
// so it reads either form without knowing which one it got. The two forms
// can coexist for a moment — the compressed file is renamed into place before
// the plain one is removed, and a crash can land between the two — and in
// that case the plain file wins. It is always complete: a sealed file is never
// written again, and the compressed file only ever appears by an atomic rename
// after its contents are synced, so both are whole and the plain one is the
// cheaper read. Because the plain file is removed only after the compressed one
// is durable, at every instant at least one of the two names exists: a reader
// that finds the plain file gone will find the compressed one.
//
// The format is zstd, from github.com/klauspost/compress: pure Go, no cgo, and
// already in the module graph through the Prometheus client. On the raw Claude
// transcript in the bug report zstd -19 gave 3.0x where gzip -9 gave 2.5x; on
// the daemon's own sanitized logs the encoder here matches gzip -9 on Claude
// harness logs and beats it several times over on repaint-heavy ones, and it
// decodes far faster. The zstd CLI reads the files by hand (`zstd -dc
// file.log.zst`). Memory is kept low on both sides: one encoder, reused,
// running on one goroutine with a small window; decoders in low-memory,
// single-goroutine mode, with the window they will accept capped.
//
// Governing: ADR-0007 (durable log rotation, as amended for sealed
// compression), ADR-0033 (the raw `.stream.jsonl` is compressed on seal);
// SPEC-0003 REQ "Durable Log Rotation And Compression"; SPEC-0008 REQ "Per-Run
// Logs"; GitHub https://github.com/stump-wtf/harness/issues/18.
//
// @joestump-agent 09/28/2026 - Added for
// https://github.com/stump-wtf/harness/issues/18.
package sealedlog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Ext is the suffix a sealed file gains when it is compressed.
const Ext = ".zst"

// tempInfix marks a compression in progress: the compressed bytes of
// <dir>/<base> are written to <dir>/.<base>.zst-tmp-<random> and renamed into
// place only once they are synced. The leading dot keeps a temp out of every
// glob and id parser that looks for a real log.
const tempInfix = Ext + "-tmp-"

// windowSize is the encoder's back-reference window. The default is 8 MiB,
// and every decoder must then hold 8 MiB of history: measured, a reader of an
// 8 MiB-window file retains about 9 MiB, of a 1 MiB-window file about 2 MiB.
// The ratio barely notices, because a log's repetition is local — 14.41x
// against 14.48x on 12.3 MB of real rotated Claude harness logs.
const windowSize = 1 << 20

// maxDecodeWindow is the largest window a decoder accepts. It covers our own
// files and anything `zstd -19` writes (an 8 MiB window), and refuses a file
// that would make a reader allocate far more — a hand-placed or corrupt file
// must not be able to cost the daemon hundreds of megabytes.
const maxDecodeWindow = 8 << 20

// Compressed is the name path has once compressed.
func Compressed(path string) string { return path + Ext }

// Plain strips the compressed suffix from name, reporting whether it had one.
// It works on a base name or a whole path.
func Plain(name string) (string, bool) {
	return strings.CutSuffix(name, Ext)
}

// TempOf reports whether name (a base name) is a compression temp, and if so
// the base name of the file being compressed.
func TempOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, ".")
	if !ok {
		return "", false
	}
	i := strings.LastIndex(rest, tempInfix)
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

// Open opens the log at path for reading in whichever form it is stored: the
// plain file when it exists, otherwise path+Ext through a streaming,
// low-memory decoder. When neither exists the error satisfies
// errors.Is(err, fs.ErrNotExist). Close releases the decoder too.
func Open(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	zf, zerr := os.Open(Compressed(path))
	if zerr != nil {
		if errors.Is(zerr, fs.ErrNotExist) {
			return nil, err // the plain name is the one callers asked for
		}
		return nil, zerr
	}
	dec, derr := newDecoder(zf)
	if derr != nil {
		_ = zf.Close()
		return nil, fmt.Errorf("sealedlog: open %s: %w", Compressed(path), derr)
	}
	return &decodedFile{Decoder: dec, f: zf}, nil
}

// newDecoder is a decoder for r in the low-memory configuration every reader
// uses: synchronous (no goroutines of its own), lowmem buffers, and a capped
// window.
func newDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(maxDecodeWindow),
	)
}

// decodedFile is an open compressed log: reads decode, Close releases both the
// decoder and the file.
type decodedFile struct {
	*zstd.Decoder
	f *os.File
}

func (d *decodedFile) Close() error {
	d.Decoder.Close()
	return d.f.Close()
}

// Stat describes the log at path in whichever form exists, the plain file
// first. The size is the stored size, not the decoded one; the modification
// time survives compression (Compress copies it), so a reader that skips old
// files by mtime sees the same answer either way.
func Stat(path string) (fs.FileInfo, error) {
	info, err := os.Stat(path)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return info, err
	}
	if zinfo, zerr := os.Stat(Compressed(path)); zerr == nil || !errors.Is(zerr, fs.ErrNotExist) {
		return zinfo, zerr
	}
	return nil, err
}

// Missing reports whether neither form of path exists. Any other stat error
// counts as present: a log that cannot be stat'd has not been shown to be
// gone, and "gone" is what callers act on (a record reads log_pruned).
func Missing(path string) bool {
	_, err := Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// Remove deletes both forms of path. Best-effort, like every log removal in
// the daemon: a failure is ignored.
func Remove(path string) {
	_ = os.Remove(path)
	_ = os.Remove(Compressed(path))
}

// Tail returns up to the last limit bytes of the log at path, decoded, and
// whether anything before them was cut. A plain file is read from its end; a
// compressed one is streamed through a window of limit bytes, so memory stays
// at limit whatever the file's size.
func Tail(path string, limit int64) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err == nil {
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return nil, false, err
		}
		off := max(info.Size()-limit, 0)
		buf, err := io.ReadAll(io.NewSectionReader(f, off, info.Size()-off))
		return buf, off > 0, err
	}
	r, err := Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = r.Close() }()
	return tailBytes(r, limit)
}

// tailBytes keeps the last limit bytes of r.
func tailBytes(r io.Reader, limit int64) ([]byte, bool, error) {
	window := make([]byte, 0, limit)
	chunk := make([]byte, 32<<10)
	cut := false
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			window = append(window, chunk[:n]...)
			if over := int64(len(window)) - limit; over > 0 {
				window = window[:copy(window, window[over:])]
				cut = true
			}
		}
		if errors.Is(err, io.EOF) {
			return window, cut, nil
		}
		if err != nil {
			return window, cut, err
		}
	}
}

// TailLines returns the last n lines of the log at path, decoded, where a line
// is a run of bytes ending in a newline or at the end of the file. n <= 0
// returns everything. The lines are streamed through a ring of n, so reading
// the tail of a compressed log costs n lines of memory rather than the file.
// A missing log is an error satisfying errors.Is(err, fs.ErrNotExist).
func TailLines(path string, n int) ([]byte, error) {
	r, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	if n <= 0 {
		return io.ReadAll(r)
	}
	return tailLines(r, n)
}

// tailLines keeps the last n lines of r.
func tailLines(r io.Reader, n int) ([]byte, error) {
	ring := make([][]byte, n)
	next, count := 0, 0
	br := bufio.NewReaderSize(r, 32<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			ring[next] = append(ring[next][:0], line...)
			next = (next + 1) % n
			count++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	start := 0
	if count >= n {
		start = next
	}
	for i := range min(count, n) {
		out.Write(ring[(start+i)%n])
	}
	return out.Bytes(), nil
}
