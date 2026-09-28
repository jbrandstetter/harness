package supervisor

// Governing: ADR-0007 (State & scrollback ownership) — "the daemon tees raw
// PTY output to a rotating log file per harness under
// $XDG_STATE_HOME/harness/logs/<name>.log (size/age rotation)"; SPEC-0003 REQ
// "Lifecycle Events" observability. This backs `harness logs <name>` for live
// and dead harnesses alike, independent of the in-memory ring (ADR-0003).
//
// A rotated backup is sealed — nothing writes it again — so it is compressed
// in the background to <name>-<stamp>.log.zst (ADR-0007 as amended; SPEC-0003
// REQ "Durable Log Rotation And Compression"). The active file never is. Every
// reader of a backup goes through internal/sealedlog, which reads either form.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/sealedlog"
)

// rotatedStampLayout is the timestamp suffix stamped onto rotated log files.
// Kept in one place so rotation and pruning agree on the exact shape and a
// pruning harness only ever matches its own backups.
const rotatedStampLayout = "20060102T150405.000"

// LogConfig tunes per-harness log rotation. Zero values fall back to defaults
// in newRotatingLog.
type LogConfig struct {
	// Dir is the directory logs live in (…/harness/logs).
	Dir string
	// MaxBytes rotates the active file once it would exceed this size.
	MaxBytes int64
	// MaxAge rotates the active file once it is older than this.
	MaxAge time.Duration
	// MaxBackups caps how many rotated files are retained per harness.
	MaxBackups int
	// Sealer compresses each rotated backup in the background once it is
	// renamed aside (ADR-0007 as amended). Nil leaves backups uncompressed:
	// `[daemon] compress_logs = false`, and every test that does not ask.
	Sealer *sealedlog.Compressor
}

const (
	defaultMaxBytes   = 8 << 20 // 8 MiB
	defaultMaxAge     = 24 * time.Hour
	defaultMaxBackups = 5
)

// rotatingLog is an io.WriteCloser that appends sanitized history lines to
// <dir>/<name>.log, rotating by size or age. Rotated files are renamed with a
// timestamp suffix and pruned to MaxBackups. It is safe for concurrent writes
// (the PTY reader is the only writer in practice, but rotation and Close may
// race with it).
type rotatingLog struct {
	name string
	cfg  LogConfig

	mu       sync.Mutex
	f        *os.File
	size     int64
	openedAt time.Time
}

// newRotatingLog opens (creating parents) the active log file for name.
func newRotatingLog(name string, cfg LogConfig) (*rotatingLog, error) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = defaultMaxAge
	}
	if cfg.MaxBackups <= 0 {
		cfg.MaxBackups = defaultMaxBackups
	}
	rl := &rotatingLog{name: name, cfg: cfg}
	// Create the log file's own parent, not just cfg.Dir: a project harness's
	// name is namespaced ("reduit/agent"), so its log lives one level down at
	// <dir>/reduit/agent.log (ADR-0009; SPEC-0004 REQ "Project Naming And
	// Namespacing").
	if err := os.MkdirAll(filepath.Dir(rl.path()), 0o755); err != nil {
		return nil, fmt.Errorf("supervisor: create log dir: %w", err)
	}
	if err := rl.open(); err != nil {
		return nil, err
	}
	return rl, nil
}

// path is the active log file path for this harness.
func (rl *rotatingLog) path() string {
	return filepath.Join(rl.cfg.Dir, rl.name+".log")
}

// open opens the active file for appending and records its current size/age.
func (rl *rotatingLog) open() error {
	f, err := os.OpenFile(rl.path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("supervisor: open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("supervisor: stat log: %w", err)
	}
	rl.f = f
	rl.size = info.Size()
	rl.openedAt = time.Now()
	return nil
}

// Write appends p, rotating first if the active file has hit its size or age
// bound. It implements io.Writer.
func (rl *rotatingLog) Write(p []byte) (int, error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.f == nil {
		return 0, os.ErrClosed
	}
	if rl.size+int64(len(p)) > rl.cfg.MaxBytes || time.Since(rl.openedAt) > rl.cfg.MaxAge {
		if err := rl.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := rl.f.Write(p)
	rl.size += int64(n)
	if err != nil {
		return n, fmt.Errorf("supervisor: write log: %w", err)
	}
	return n, nil
}

// rotate closes the active file, renames it with a timestamp suffix, prunes old
// backups, and opens a fresh active file. Caller holds rl.mu.
func (rl *rotatingLog) rotate() error {
	if rl.f != nil {
		_ = rl.f.Close()
		rl.f = nil
	}
	// Only rename if there is content to preserve.
	if rl.size > 0 {
		rotated := rl.rotatedPath(time.Now())
		if err := os.Rename(rl.path(), rotated); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("supervisor: rotate log: %w", err)
		}
		rl.pruneBackups()
	}
	return rl.open()
}

// rotatedPath is the name the active file is renamed to at now: its rotation
// stamp, moved on a millisecond at a time past any backup that already holds
// it. Two rotations inside one millisecond used to rename over the first
// backup; with compression the collision is worse, because the second backup's
// plain file would sit beside the first one's .zst under one name, and a reader
// must never be left to guess which of two different logs a name means.
func (rl *rotatingLog) rotatedPath(now time.Time) string {
	for range 1000 {
		p := filepath.Join(rl.cfg.Dir, fmt.Sprintf("%s-%s.log", rl.name, now.Format(rotatedStampLayout)))
		if sealedlog.Missing(p) {
			return p
		}
		now = now.Add(time.Millisecond)
	}
	return filepath.Join(rl.cfg.Dir, fmt.Sprintf("%s-%s.log", rl.name, now.Format(rotatedStampLayout)))
}

// pruneBackups deletes the oldest rotated backups beyond MaxBackups, and
// queues every retained backup still in plain form for compression: the one
// just rotated, and any an earlier daemon left behind (a crash, a failed
// compression, or compression switched on after being off). Best-effort:
// prune failures never fail a write, and the compression itself happens on the
// Sealer's goroutine, never on the writer's.
func (rl *rotatingLog) pruneBackups() {
	backups := listBackups(rl.cfg.Dir, rl.name)
	drop := max(len(backups)-rl.cfg.MaxBackups, 0)
	for _, b := range backups[:drop] {
		sealedlog.Remove(b.path)
	}
	for _, b := range backups[drop:] {
		if b.plain {
			rl.cfg.Sealer.Seal(b.path)
		}
	}
}

// backup is one rotation of a harness's durable log.
type backup struct {
	// path is the backup's plain name, <name>-<stamp>.log, whichever form is
	// on disk; internal/sealedlog resolves it.
	path string
	// plain is set when the uncompressed form is present — not yet
	// compressed, mid-compression, or left beside its .zst by a crash.
	plain bool
}

// listBackups returns the named harness's rotated backups, oldest first, one
// entry per rotation whatever form it is in. Only THIS harness's own backups
// count. A bare glob on `<name>-*.log` also matches sibling harnesses whose
// name shares ours as a prefix (listing "web" would otherwise sweep up
// "web-api.log" and "web-api-<stamp>.log.zst"); isBackupOf keeps only files
// whose suffix parses as our rotation timestamp.
func listBackups(dir, name string) []backup {
	base := filepath.Join(dir, name)
	var files []string
	for _, pattern := range []string{base + "-*.log", base + "-*.log" + sealedlog.Ext} {
		globbed, err := filepath.Glob(pattern)
		if err != nil {
			return nil
		}
		files = append(files, globbed...)
	}
	byPath := map[string]*backup{}
	var out []*backup
	for _, f := range files {
		if !isBackupOf(name, f) {
			continue
		}
		plainPath, compressed := sealedlog.Plain(f)
		b := byPath[plainPath]
		if b == nil {
			b = &backup{path: plainPath}
			byPath[plainPath] = b
			out = append(out, b)
		}
		b.plain = b.plain || !compressed
	}
	// The timestamp suffix sorts chronologically, so the oldest are first.
	slices.SortFunc(out, func(a, b *backup) int { return strings.Compare(a.path, b.path) })
	backups := make([]backup, len(out))
	for i, b := range out {
		backups[i] = *b
	}
	return backups
}

// isBackupOf reports whether path is a rotated backup of the named harness,
// in either form: `<name>-<stamp>.log` or `<name>-<stamp>.log.zst`.
// The comparison uses the BASE of the (possibly namespaced) harness name: a
// project harness "reduit/agent" rotates to <dir>/reduit/agent-<stamp>.log, so
// after filepath.Base only "agent-<stamp>" remains to match (ADR-0009;
// SPEC-0004 REQ "Project Naming And Namespacing"). Cross-harness confusion is
// impossible because callers glob within the name's own directory and the
// suffix must parse as our exact rotation timestamp.
func isBackupOf(name, path string) bool {
	base, _ := sealedlog.Plain(filepath.Base(path))
	mid, ok := strings.CutSuffix(base, ".log")
	if !ok {
		return false // no .log suffix
	}
	stamp, ok := strings.CutPrefix(mid, filepath.Base(name)+"-")
	if !ok {
		return false // not `<name>-…`
	}
	_, err := time.Parse(rotatedStampLayout, stamp)
	return err == nil
}

// removeLogArtifacts deletes a harness's on-disk log tree: the active
// <dir>/<name>.log, every rotated backup in either form (and the temp of a
// compression a crash interrupted), and — for a namespaced project harness —
// the project's log subdirectory if it is now empty. The Manager calls this
// when a project harness is deregistered so torn-down projects do not leak
// unreachable log files forever (SPEC-0004 REQ "Tear Down": the daemon retains
// no record of the project afterward). Best-effort: removal failures are
// ignored, exactly like pruneBackups.
func removeLogArtifacts(dir, name string) {
	active := filepath.Join(dir, name+".log")
	_ = os.Remove(active)
	for _, b := range listBackups(dir, name) {
		sealedlog.Remove(b.path)
	}
	if entries, err := os.ReadDir(filepath.Dir(active)); err == nil {
		for _, e := range entries {
			if orig, ok := sealedlog.TempOf(e.Name()); ok && isBackupOf(name, orig) {
				_ = os.Remove(filepath.Join(filepath.Dir(active), e.Name()))
			}
		}
	}
	if filepath.Dir(name) != "." {
		// Namespaced name: drop the per-project directory when empty (os.Remove
		// refuses a non-empty directory, which is exactly what we want).
		_ = os.Remove(filepath.Dir(active))
	}
}

// Close closes the active file. Implements io.Closer.
func (rl *rotatingLog) Close() error {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.f == nil {
		return nil
	}
	err := rl.f.Close()
	rl.f = nil
	if err != nil {
		return fmt.Errorf("supervisor: close log: %w", err)
	}
	return nil
}
