package sealedlog

// Compression
//
// A Compressor is one background goroutine and one reused zstd encoder. Callers
// hand it paths with Seal, which never blocks: the PTY reader and the
// supervisor's actor loop must not wait on a disk, least of all one whose
// fsync takes seconds under load. The worker takes one file at a time, so the
// daemon never holds more than one encoder's worth of compression state
// whatever the number of harnesses.
//
// One file is compressed like this, and a crash at any step leaves either the
// plain file alone, or both forms whole:
//
//  1. encode the plain file into a temp beside it (.<base>.zst-tmp-*), with the
//     plain file's permissions;
//  2. fsync the temp, close it, and give it the plain file's mtime;
//  3. check the plain file has not changed since it was opened — a file still
//     growing is not sealed, whatever the caller thought, and is left alone;
//  4. rename the temp to <file>.zst and fsync the directory, so the new name
//     is durable;
//  5. only then remove the plain file — unless a prune removed it first, in
//     which case the compressed copy is removed instead.
//
// A failure logs a warning and leaves the plain file where it was. Nothing is
// lost: readers read either form, and the next boot sweep or prune queues it
// again.
//
// Governing: ADR-0007 (as amended for sealed compression); SPEC-0003 REQ
// "Durable Log Rotation And Compression"; SPEC-0008 REQ "Per-Run Logs".

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/klauspost/compress/zstd"
)

// level is the encoder level. Measured on 12.3 MB of real rotated Claude
// harness logs: SpeedDefault (zstd level 3) 11.7x, SpeedBetterCompression
// (about level 7) 14.4x, SpeedBestCompression 15.5x. Better costs about 40%
// more CPU than default — 21 ms against 15 ms for those 12 MB — for a quarter
// more ratio; Best costs four times the CPU and several times the transient
// memory for 8% more. This runs on a daemon supervising agents, once per
// sealed file, so Better is the knee.
const level = zstd.SpeedBetterCompression

// copyBuf is how much of a plain file is read per step. The stop check sits
// between steps, so it also bounds how long Close waits on a file in flight.
const copyBuf = 64 << 10

// errAborted reports a compression Close interrupted. The temp is removed and
// the plain file kept; the next boot sweeps it up.
var errAborted = errors.New("sealedlog: compression aborted by shutdown")

// errGrowing reports a file that changed while it was being compressed.
var errGrowing = errors.New("sealedlog: file changed while it was compressed; it is not sealed")

// syncFile and syncDir are the two durability calls, variables so a test
// binary can skip them (SkipSyncForTesting) and a test can fail them.
var (
	syncFile = func(f *os.File) error { return f.Sync() }
	syncDir  = func(dir string) error {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer func() { _ = d.Close() }()
		return d.Sync()
	}
)

// Compressor compresses sealed files in the background. The zero value is not
// usable; build one with NewCompressor. A nil *Compressor is valid and does
// nothing, which is how compression is switched off.
type Compressor struct {
	mu      sync.Mutex
	idle    *sync.Cond
	queue   []string
	pending map[string]bool
	busy    bool
	closed  bool

	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	// Worker-owned: the encoder is built on the first file and reused for
	// every file after it, and the copy buffer likewise.
	enc *zstd.Encoder
	buf []byte
}

// NewCompressor starts a Compressor's worker. Close stops it.
func NewCompressor() *Compressor {
	c := &Compressor{
		pending: map[string]bool{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	c.idle = sync.NewCond(&c.mu)
	go c.run()
	return c
}

// Seal queues sealed files for compression and returns at once. An empty path
// is ignored, and so is one already queued. A path whose file has gone by the
// time the worker reaches it (already compressed, or pruned) is skipped.
//
// The caller vouches that nothing will write to path again. The worker checks
// that too — a file that changes while it is being compressed is left plain —
// but it cannot see a writer that is merely idle.
func (c *Compressor) Seal(paths ...string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	for _, p := range paths {
		if p == "" || c.pending[p] {
			continue
		}
		c.pending[p] = true
		c.queue = append(c.queue, p)
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Wait blocks until everything queued so far has been processed, or the
// Compressor is closed.
func (c *Compressor) Wait() {
	if c == nil {
		return
	}
	c.mu.Lock()
	for !c.closed && (len(c.queue) > 0 || c.busy) {
		c.idle.Wait()
	}
	c.mu.Unlock()
}

// Close stops the worker: queued files are dropped, and a file in flight is
// abandoned between two reads, its temp removed and its plain file kept. It
// does not wait for a queue to drain, because a daemon shutdown must not wait
// on compressing a backlog; whatever was dropped is still a plain file, which
// the next boot's sweep queues again. Close is idempotent.
func (c *Compressor) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.queue = nil
	clear(c.pending)
	c.idle.Broadcast()
	c.mu.Unlock()
	if !already {
		close(c.stop)
	}
	<-c.done
}

func (c *Compressor) run() {
	defer close(c.done)
	defer c.release()
	for {
		path, ok := c.take()
		if !ok {
			select {
			case <-c.wake:
				continue
			case <-c.stop:
				return
			}
		}
		err := c.compress(path)
		if err != nil && !errors.Is(err, errAborted) {
			log.Warn("could not compress a sealed log; it stays uncompressed and is retried at the next boot or prune",
				"path", path, "err", err)
		}
		c.mu.Lock()
		c.busy = false
		delete(c.pending, path)
		c.idle.Broadcast()
		c.mu.Unlock()
	}
}

// take pops the next queued path, marking the worker busy.
func (c *Compressor) take() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || len(c.queue) == 0 {
		return "", false
	}
	p := c.queue[0]
	c.queue = c.queue[1:]
	c.busy = true
	return p, true
}

// release frees the encoder.
func (c *Compressor) release() {
	if c.enc != nil {
		_ = c.enc.Close()
		c.enc = nil
	}
}

// CompressFile compresses one sealed file synchronously, with an encoder of
// its own: path becomes path+Ext, durably, and path is removed. A path that
// does not exist is not an error. The daemon uses a Compressor; this is for
// tests and tools that need a compressed log on disk.
func CompressFile(path string) error {
	c := &Compressor{stop: make(chan struct{})}
	defer c.release()
	return c.compress(path)
}

// compress seals one file (see the file comment for the steps).
func (c *Compressor) compress(path string) (err error) {
	src, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // compressed already, or pruned
	}
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}

	dst := Compressed(path)
	if _, err := os.Stat(dst); err == nil {
		// Both forms. The usual cause is a crash between step 4 and step 5,
		// and then the compressed file is whole; but it is checked before the
		// only other copy goes, and rewritten if it does not decode to the
		// plain file.
		if n, derr := decodedSize(dst); derr == nil && n == info.Size() {
			return removeIfPresent(path)
		}
	}

	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	removeTemps(dir, base)
	tmp, err := os.CreateTemp(dir, "."+base+tempInfix+"*")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	read, err := c.encode(tmp, src)
	if err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return fmt.Errorf("sync %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The mtime is when the log was last written, which a reader that skips
	// old files by mtime (ReadLifecycle's since) depends on. A zero atime
	// leaves the access time alone.
	if err := os.Chtimes(tmp.Name(), time.Time{}, info.ModTime()); err != nil {
		return err
	}
	now, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // pruned while we worked; the temp goes with it
	}
	if err != nil {
		return err
	}
	if now.Size() != read || !now.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("%w: %s", errGrowing, path)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	keep = true
	if err := syncDir(dir); err != nil && !unsupportedSync(err) {
		// The new name may not be durable yet, so the plain file stays: two
		// whole copies until the next sweep beats one that a power cut could
		// take.
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	// Only a prune removes a sealed plain file out from under the worker, and
	// a prune means the log is to go. If it went while this was compressing,
	// the compressed copy goes too; checked after the rename, so whichever
	// order the prune's two removals and this rename interleave in, no
	// orphaned .zst outlives the prune that meant to delete it.
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return removeIfPresent(dst)
	}
	return removeIfPresent(path)
}

// encode streams src into dst through the reused encoder, returning the bytes
// read. It stops between reads when the Compressor is closing.
func (c *Compressor) encode(dst io.Writer, src io.Reader) (int64, error) {
	if c.enc == nil {
		enc, err := zstd.NewWriter(nil,
			zstd.WithEncoderConcurrency(1),
			zstd.WithLowerEncoderMem(true),
			zstd.WithWindowSize(windowSize),
			zstd.WithEncoderLevel(level),
		)
		if err != nil {
			return 0, err
		}
		c.enc = enc
	}
	if c.buf == nil {
		c.buf = make([]byte, copyBuf)
	}
	c.enc.Reset(dst)
	var read int64
	for {
		select {
		case <-c.stop:
			return read, errAborted
		default:
		}
		n, err := src.Read(c.buf)
		if n > 0 {
			read += int64(n)
			if _, werr := c.enc.Write(c.buf[:n]); werr != nil {
				return read, werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return read, err
		}
	}
	return read, c.enc.Close()
}

// decodedSize decodes the whole of a compressed file and counts its bytes.
func decodedSize(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	dec, err := newDecoder(f)
	if err != nil {
		return 0, err
	}
	defer dec.Close()
	return io.Copy(io.Discard, dec)
}

// removeTemps deletes the temps a crashed compression of base left in dir.
// Only the worker compresses, one file at a time, so no live temp for base
// can exist while it is here.
func removeTemps(dir, base string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if orig, ok := TempOf(e.Name()); ok && orig == base {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// removeIfPresent removes path, treating an already-missing file as done.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// unsupportedSync reports a directory fsync the filesystem does not do. The
// rename has happened either way; there is nothing a retry would change.
func unsupportedSync(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EBADF)
}
