// Package attach is the daemon-side data plane: one github.com/charmbracelet/x/vt
// terminal emulator plus a byte-bounded scrollback ring of raw PTY bytes per
// running harness, fed by the supervisor's raw PTY output (via the Manager
// ExtraOut hook), and the attach sessions that stream a
// snapshot-then-tail-then-live view of it to clients with coalesce-to-snapshot
// backpressure.
//
// Governing: SPEC-0002 (daemon-protocol) REQ "Attach Session" and REQ
// "Backpressure Isolation"; ADR-0003 (native multiplexer: one x/vt emulator per
// harness; smallest-attached-wins resize); ADR-0007 (byte-bounded ring +
// on-disk log; the PTY reader never blocks on a slow client — it repaints from
// the current screen); ADR-0008 (read-only attach).
package attach

import (
	"bytes"
	"math/bits"
	"strconv"
	"sync"
	"unicode/utf8"
)

// Scrollback ring limits (ADR-0007 "Scrollback, per harness").
//
// The byte budget is the bound that holds memory down. The ring used to be
// capped at 10,000 lines and nothing else, and Claude Code's stream-json lines
// run 2–34 KB on average and up to 4 MB, so a full ring was 20–340 MB per
// harness (GitHub https://github.com/stump-wtf/harness/issues/18). The line cap
// stays as a secondary bound, and the per-line cap stops one enormous line from
// evicting everything else.
const (
	// DefaultRingBytes is one harness's default storage budget. Its only
	// reader is attach replay (TUI scroll and search read the on-disk log),
	// and the TUI's client-side x/vt parses replayed bytes at single-digit
	// MB/s, so every retained byte costs attach latency on every attach and
	// every dashboard peek. 1 MiB is about 13,000 lines of 80-column text
	// (roughly the old 10,000-line default for a shell) and replays in well
	// under a quarter second.
	DefaultRingBytes = 1 << 20
	// DefaultRingLines is the secondary bound: completed lines retained,
	// whatever their size (ADR-0007: "default N lines, configurable").
	DefaultRingLines = 10000
	// DefaultRingLineBytes caps one line. A longer line keeps its head and
	// ends in a truncation marker. It matches the claude-code peek
	// formatter's own line cap (adapter.streamJSONLineCap), past which that
	// formatter already shows the raw bytes, and it keeps whole the typical
	// stream-json line.
	DefaultRingLineBytes = 64 << 10
)

// Storage chunking. The ring stores bytes in fixed-size chunks sized so a full
// budget is about ringChunksPerBudget of them: few enough that attach replay
// fits a session queue with room to spare, large enough that storage costs
// one allocation per chunk rather than one per line.
const (
	ringChunksPerBudget = 64
	minRingChunk        = 4 << 10
	maxRingChunk        = 1 << 20
)

// maxTailFrames bounds how many frames one attach's scrollback replay queues.
// A budget large enough to hit the chunk ceiling replays only its newest
// maxTailFrames chunks; every other budget replays the whole ring. Attach
// asserts the snapshot plus this many frames fit in a fresh session queue.
const maxTailFrames = 128

// RingLimits bounds one harness's scrollback ring. A zero field takes its
// default.
type RingLimits struct {
	// Lines caps the completed lines retained (DefaultRingLines).
	Lines int
	// Bytes is the storage budget (DefaultRingBytes).
	Bytes int
	// LineBytes caps a single line, marker excluded. Zero derives it:
	// DefaultRingLineBytes, or a quarter of Bytes when that is smaller.
	LineBytes int
	// chunkSize overrides the derived storage chunk size. Tests shrink it
	// to cross chunk boundaries with a few bytes.
	chunkSize int
}

// withDefaults fills zero and negative fields.
func (l RingLimits) withDefaults() RingLimits {
	if l.Lines <= 0 {
		l.Lines = DefaultRingLines
	}
	if l.Bytes <= 0 {
		l.Bytes = DefaultRingBytes
	}
	if l.LineBytes <= 0 {
		l.LineBytes = max(min(DefaultRingLineBytes, l.Bytes/4), 1)
	}
	if l.chunkSize <= 0 {
		l.chunkSize = ringChunkSize(l.Bytes)
	}
	return l
}

// ringChunkSize is budget/ringChunksPerBudget rounded up to a power of two and
// clamped to [minRingChunk, maxRingChunk].
func ringChunkSize(budget int) int {
	c := max(budget/ringChunksPerBudget, 1)
	c = 1 << bits.Len(uint(c-1))
	return min(max(c, minRingChunk), maxRingChunk)
}

// ringChunk is one storage chunk. buf's capacity is the chunk size and it is
// only ever appended to, so bytes written once never change: that is what lets
// tailFrames hand out views instead of copies.
type ringChunk struct {
	buf []byte
	// lent marks a chunk tailFrames has handed out. A session may still be
	// sending it, so it is never recycled; the GC frees it once both the
	// ring and every session have let go.
	lent bool
}

// ring is a bounded, line-oriented scrollback buffer of raw PTY bytes. It keeps
// the newest completed lines (each including its trailing '\n') plus the
// current partial line, and evicts the oldest whole lines to stay within
// lim.Lines lines and lim.Bytes of storage. tailFrames hands out the
// retained bytes for the scrollback portion of an attach (SPEC-0002 REQ
// "Attach Session": "a bounded tail of scrollback").
//
// Offsets are absolute byte positions in the stream since the ring was made,
// so eviction is a pointer move: start advances past the oldest line, and a
// chunk wholly behind start is released. Nothing is copied down.
type ring struct {
	lim       RingLimits
	maxChunks int

	mu       sync.Mutex
	chunks   []ringChunk // oldest first; all but the last are full
	chunkArr []ringChunk // backing array chunks slides along (openChunk)
	// spares are released, never-lent chunks kept for reuse, so a full ring
	// with nobody attached writes without allocating. A chunk moves from
	// chunks to spares and back, so the two together stay within maxChunks.
	spares [][]byte
	base   int64 // offset of chunks[0].buf[0]
	start  int64 // offset of the oldest retained byte (a line start)
	end    int64 // offset one past the newest byte

	// lineEnds is a circular queue of completed lines' end offsets, oldest
	// at lhead. It grows to at most lim.Lines entries.
	lineEnds []int64
	lhead    int
	lcount   int

	lineStart int64 // offset where the current partial line starts
	dropped   int   // bytes cut from the current partial line so far
}

// newRing builds a ring bounded by lim (zero fields take defaults). It
// allocates no storage until the first Write.
func newRing(lim RingLimits) *ring {
	lim = lim.withDefaults()
	return &ring{lim: lim, maxChunks: max(lim.Bytes/lim.chunkSize, 1)}
}

// Write appends raw PTY bytes, splitting on newlines, truncating a line past
// the per-line cap, and evicting the oldest lines to stay within the limits.
// It never returns an error and never blocks on anything but its own mutex
// (ADR-0007 backpressure: writing scrollback must not stall the PTY reader).
func (r *ring) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(p) > 0 {
		seg, rest, complete := p, []byte(nil), false
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			seg, rest, complete = p[:i+1], p[i+1:], true
		}
		r.writeLine(seg, complete)
		p = rest
	}
}

// writeLine stores one segment of the current line; complete means it ends in
// the line's '\n'. A line that outgrows lim.LineBytes keeps its head, drops the
// rest, and on completion gets truncMarker in place of the dropped bytes.
func (r *ring) writeLine(seg []byte, complete bool) {
	if r.dropped == 0 {
		room := r.lim.LineBytes - int(r.end-r.lineStart)
		if len(seg) <= room {
			r.appendBytes(seg)
			if complete {
				r.completeLine()
			}
			return
		}
		keep := runeSafeCut(seg, room)
		r.appendBytes(seg[:keep])
		r.dropped = len(seg) - keep
	} else {
		r.dropped += len(seg)
	}
	if complete {
		var buf [64]byte
		r.appendBytes(truncMarker(buf[:0], r.dropped))
		r.dropped = 0
		r.completeLine()
	}
}

// truncMarker appends the marker that ends a truncated line to dst. It opens
// with CAN, which aborts any escape sequence the cut left open (an OSC
// hyperlink cut mid-URL would otherwise swallow everything after it), then
// resets SGR so the cut line's colors do not bleed into the next. It ends in
// CRLF, like the PTY's own line endings, so the next line starts at column 0.
// n counts every byte of the line that was not kept, line ending included.
func truncMarker(dst []byte, n int) []byte {
	dst = append(dst, "\x18\x1b[m…[harness: truncated "...)
	dst = strconv.AppendInt(dst, int64(n), 10)
	return append(dst, " bytes]\r\n"...)
}

// runeSafeCut returns n, reduced so b[:n] does not end partway through a UTF-8
// sequence. It looks only inside b: a sequence split across two Writes at the
// cap is left for the marker's CAN to discard.
func runeSafeCut(b []byte, n int) int {
	for i := n; i > 0 && i > n-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i-1]) {
			if !utf8.FullRune(b[i-1 : n]) {
				return i - 1
			}
			return n
		}
	}
	return n
}

// completeLine records the line ending at end, evicting the oldest line first
// when the line cap is reached.
func (r *ring) completeLine() {
	if r.lcount == r.lim.Lines {
		r.evictLine()
	}
	if r.lcount == len(r.lineEnds) {
		n := min(max(2*len(r.lineEnds), 64), r.lim.Lines)
		next := make([]int64, n)
		for i := range r.lcount {
			next[i] = r.lineEnd(i)
		}
		r.lineEnds, r.lhead = next, 0
	}
	r.lineEnds[(r.lhead+r.lcount)%len(r.lineEnds)] = r.end
	r.lcount++
	r.lineStart = r.end
}

// lineEnd returns the end offset of the i-th oldest completed line.
func (r *ring) lineEnd(i int) int64 {
	return r.lineEnds[(r.lhead+i)%len(r.lineEnds)]
}

// evictLine drops the oldest completed line and releases any chunk wholly
// behind it. O(1) plus the chunks released.
func (r *ring) evictLine() {
	r.start = r.lineEnds[r.lhead]
	r.lhead = (r.lhead + 1) % len(r.lineEnds)
	r.lcount--
	for len(r.chunks) > 0 && r.start >= r.base+int64(r.lim.chunkSize) {
		c := r.chunks[0]
		r.chunks[0] = ringChunk{}
		r.chunks = r.chunks[1:]
		r.base += int64(r.lim.chunkSize)
		if !c.lent && len(r.chunks)+len(r.spares) < r.maxChunks {
			r.spares = append(r.spares, c.buf[:0])
		}
	}
}

// appendBytes copies b into storage, opening chunks as they fill.
func (r *ring) appendBytes(b []byte) {
	for len(b) > 0 {
		if n := len(r.chunks); n == 0 || len(r.chunks[n-1].buf) == r.lim.chunkSize {
			r.openChunk()
		}
		c := &r.chunks[len(r.chunks)-1]
		k := min(len(b), r.lim.chunkSize-len(c.buf))
		c.buf = append(c.buf, b[:k]...)
		r.end += int64(k)
		b = b[k:]
	}
}

// openChunk appends an empty chunk, first evicting the oldest lines until the
// chunk fits the byte budget. When no completed line is left to evict, the
// current partial line is what holds the storage; it is capped at
// lim.LineBytes plus a marker, so the overshoot is bounded and brief.
func (r *ring) openChunk() {
	for len(r.chunks) >= r.maxChunks && r.lcount > 0 {
		r.evictLine()
	}
	var buf []byte
	if n := len(r.spares); n > 0 {
		buf, r.spares[n-1] = r.spares[n-1], nil
		r.spares = r.spares[:n-1]
	} else {
		buf = make([]byte, 0, r.lim.chunkSize)
	}
	if len(r.chunks) == 0 {
		r.base = r.end
	}
	// chunks is a window sliding along chunkArr: eviction advances its start,
	// this advances its end. At the array's end, slide the window back to
	// the front rather than let append allocate a new array — amortized
	// O(1), since the array holds twice the budget's chunks.
	if len(r.chunks) == cap(r.chunks) {
		if 2*len(r.chunks) > len(r.chunkArr) {
			r.chunkArr = make([]ringChunk, max(2*len(r.chunks), 2*r.maxChunks, 4))
		}
		n := copy(r.chunkArr, r.chunks)
		clear(r.chunkArr[n:]) // the stale tail would pin evicted chunks
		r.chunks = r.chunkArr[:n]
	}
	r.chunks = append(r.chunks, ringChunk{buf: buf})
}

// tailFrames returns the retained scrollback, oldest first, as read-only views
// into the ring's storage: one frame per chunk it spans, so no frame is larger
// than a chunk and nothing is copied. The views stay valid after the ring moves
// on, because a chunk's written bytes never change and a lent chunk is never
// recycled. At most maxTailFrames frames are returned; past that the replay
// starts at the first line boundary that fits.
func (r *ring) tailFrames() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	cs := int64(r.lim.chunkSize)
	from := r.start
	if limit := int64(maxTailFrames-1) * cs; r.end-from > limit {
		from = r.lineBoundaryFrom(r.end - limit)
	}
	if from >= r.end {
		return nil
	}
	first := int((from - r.base) / cs)
	frames := make([][]byte, 0, len(r.chunks)-first)
	for i := first; i < len(r.chunks); i++ {
		c := &r.chunks[i]
		lo := 0
		if chunkStart := r.base + int64(i)*cs; from > chunkStart {
			lo = int(from - chunkStart)
		}
		if lo >= len(c.buf) {
			continue
		}
		c.lent = true
		frames = append(frames, c.buf[lo:len(c.buf):len(c.buf)])
	}
	return frames
}

// lineBoundaryFrom returns the first line start at or after off, or off itself
// when off falls inside the current partial line.
func (r *ring) lineBoundaryFrom(off int64) int64 {
	lo, hi := 0, r.lcount
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if r.lineEnd(mid) < off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < r.lcount {
		return r.lineEnd(lo)
	}
	return off
}

// Lines reports how many completed lines are currently retained.
func (r *ring) Lines() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lcount
}

// Len reports how many bytes are retained: completed lines plus the partial.
func (r *ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return int(r.end - r.start)
}

// allocated reports the storage the ring holds, spares included: the figure
// the byte budget bounds.
func (r *ring) allocated() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.chunks {
		n += cap(c.buf)
	}
	for _, b := range r.spares {
		n += cap(b)
	}
	return n
}
