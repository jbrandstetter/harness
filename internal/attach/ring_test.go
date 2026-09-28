package attach

// Governing: ADR-0007 (byte-bounded scrollback ring with a per-line cap and a
// secondary line cap, configurable) and SPEC-0002 REQ "Attach Session" ("a
// bounded tail of scrollback").

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// tailString joins the ring's tail frames the way a client sees them.
func tailString(r *ring) string {
	return string(bytes.Join(r.tailFrames(), nil))
}

func TestRingTailAndEviction(t *testing.T) {
	tests := []struct {
		name     string
		lim      RingLimits
		writes   []string
		wantTail string
		wantN    int // completed lines retained
	}{
		{
			name:     "under cap keeps everything",
			lim:      RingLimits{Lines: 5},
			writes:   []string{"a\n", "b\n", "c\n"},
			wantTail: "a\nb\nc\n",
			wantN:    3,
		},
		{
			name:     "evicts oldest past the line cap",
			lim:      RingLimits{Lines: 2},
			writes:   []string{"a\n", "b\n", "c\n", "d\n"},
			wantTail: "c\nd\n",
			wantN:    2,
		},
		{
			name:     "partial line preserved",
			lim:      RingLimits{Lines: 3},
			writes:   []string{"line1\n", "partial-"},
			wantTail: "line1\npartial-",
			wantN:    1,
		},
		{
			name:     "bytes split across writes coalesce into one line",
			lim:      RingLimits{Lines: 3},
			writes:   []string{"hel", "lo\n"},
			wantTail: "hello\n",
			wantN:    1,
		},
		{
			// 3-byte chunks, 9-byte budget: three chunks at most. Six
			// 3-byte lines need six chunks, so the oldest three go.
			name:     "evicts oldest past the byte budget",
			lim:      RingLimits{Bytes: 9, LineBytes: 9, chunkSize: 3},
			writes:   []string{"a1\n", "b2\n", "c3\n", "d4\n", "e5\n", "f6\n"},
			wantTail: "d4\ne5\nf6\n",
			wantN:    3,
		},
		{
			name:     "a line spanning chunks survives intact",
			lim:      RingLimits{Bytes: 64, LineBytes: 64, chunkSize: 4},
			writes:   []string{"0123456789abcdef\n", "x\n"},
			wantTail: "0123456789abcdef\nx\n",
			wantN:    2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRing(tc.lim)
			for _, w := range tc.writes {
				r.Write([]byte(w))
			}
			if got := tailString(r); got != tc.wantTail {
				t.Errorf("tail = %q, want %q", got, tc.wantTail)
			}
			if got := r.Lines(); got != tc.wantN {
				t.Errorf("Lines() = %d, want %d", got, tc.wantN)
			}
			if got := r.Len(); got != len(tc.wantTail) {
				t.Errorf("Len() = %d, want %d", got, len(tc.wantTail))
			}
		})
	}
}

func TestRingDefaults(t *testing.T) {
	r := newRing(RingLimits{})
	if r.lim.Lines != DefaultRingLines || r.lim.Bytes != DefaultRingBytes || r.lim.LineBytes != DefaultRingLineBytes {
		t.Fatalf("defaults = %+v, want lines %d, bytes %d, line bytes %d",
			r.lim, DefaultRingLines, DefaultRingBytes, DefaultRingLineBytes)
	}
	if r.lim.chunkSize != 16<<10 || r.maxChunks != 64 {
		t.Errorf("chunk = %d × %d, want 16 KiB × 64", r.lim.chunkSize, r.maxChunks)
	}
	if got := r.allocated(); got != 0 {
		t.Errorf("an unwritten ring holds %d bytes, want 0 (storage is lazy)", got)
	}
	r.Write([]byte(strings.Repeat("x\n", 10)))
	if r.Lines() != 10 {
		t.Errorf("Lines() = %d, want 10", r.Lines())
	}
	// A small budget derives a smaller line cap, so one line cannot fill it.
	if small := newRing(RingLimits{Bytes: 64 << 10}); small.lim.LineBytes != 16<<10 {
		t.Errorf("64 KiB budget: line cap = %d, want 16 KiB", small.lim.LineBytes)
	}
}

func TestRingChunkSize(t *testing.T) {
	for _, tc := range []struct{ budget, want int }{
		{64 << 10, minRingChunk},
		{1 << 20, 16 << 10},
		{1<<20 - 1, 16 << 10}, // rounds up to a power of two: 63 chunks
		{1<<20 + 1, 16 << 10}, // the remainder is dropped, so never more than 64
		{16 << 20, 256 << 10},
		{1 << 30, maxRingChunk},
	} {
		if got := ringChunkSize(tc.budget); got != tc.want {
			t.Errorf("ringChunkSize(%d) = %d, want %d", tc.budget, got, tc.want)
		}
	}
}

// benchLine is an n-byte CRLF-terminated line, like one Claude Code
// stream-json event through a PTY.
func benchLine(n int) []byte {
	return append(bytes.Repeat([]byte("x"), n-2), '\r', '\n')
}

// TestRingRetainedBytes is the report's own measurement: 40 MiB of 30 KB lines
// once retained all of it (1,365 lines is far under the 10,000-line cap). Now
// both the ring's own count and the heap stay within the byte budget.
func TestRingRetainedBytes(t *testing.T) {
	line := benchLine(30 << 10)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r := newRing(RingLimits{})
	for range (40 << 20) / len(line) {
		r.Write(line)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("40 MiB of 30 KB lines: allocated %d B (budget %d), heap delta %.2f MiB, %d lines, %d bytes retained",
		r.allocated(), DefaultRingBytes, float64(heap)/(1<<20), r.Lines(), r.Len())

	if got := r.allocated(); got > DefaultRingBytes {
		t.Errorf("ring holds %d bytes of storage, over its %d budget", got, DefaultRingBytes)
	}
	// The heap is shared with the rest of the test binary, so allow slack;
	// the unbounded ring measured 42.6 MiB here.
	if heap > 4<<20 {
		t.Errorf("heap grew %.1f MiB retaining one ring, want < 4 MiB", float64(heap)/(1<<20))
	}
	if r.Len() < DefaultRingBytes-3*r.lim.chunkSize-len(line) {
		t.Errorf("retained only %d bytes: eviction overshot the %d budget", r.Len(), DefaultRingBytes)
	}
	runtime.KeepAlive(r)
}

// TestRingFourMegabyteLine: a 4 MB line, the largest seen in a real Claude
// stream, is kept as its head plus a marker. It does not evict the lines
// before it, and it costs no more than the per-line cap.
func TestRingFourMegabyteLine(t *testing.T) {
	huge := benchLine(4 << 20)
	for _, tc := range []struct {
		name  string
		write func(r *ring)
	}{
		{"one write", func(r *ring) { r.Write(huge) }},
		{"io.Copy-sized writes", func(r *ring) {
			for off := 0; off < len(huge); off += 32 << 10 {
				r.Write(huge[off:min(off+32<<10, len(huge))])
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRing(RingLimits{})
			r.Write([]byte("before\r\n"))
			tc.write(r)
			r.Write([]byte("after\r\n"))

			want := "before\r\n" + string(huge[:DefaultRingLineBytes]) +
				string(truncMarker(nil, len(huge)-DefaultRingLineBytes)) + "after\r\n"
			if got := tailString(r); got != want {
				t.Fatalf("tail is %d bytes, want %d (head + marker between the neighbours)", len(got), len(want))
			}
			if r.Lines() != 3 {
				t.Errorf("Lines() = %d, want 3", r.Lines())
			}
			if got := r.allocated(); got > 128<<10 {
				t.Errorf("a truncated 4 MB line holds %d bytes of storage, want about one line cap", got)
			}
		})
	}
}

func TestRingTruncationMarker(t *testing.T) {
	r := newRing(RingLimits{LineBytes: 10})
	r.Write([]byte("0123456789ABCDEF\r\n")) // 18 bytes: keep 10, cut 8
	r.Write([]byte("short\r\n"))
	want := "0123456789\x18\x1b[m…[harness: truncated 8 bytes]\r\nshort\r\n"
	if got := tailString(r); got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
}

// TestRingTruncationKeepsRunes: the cut never splits a UTF-8 sequence, so the
// kept head is valid text.
func TestRingTruncationKeepsRunes(t *testing.T) {
	r := newRing(RingLimits{LineBytes: 10})
	// The cap at 10 falls one byte into "€" (3 bytes), so the head stops
	// before it.
	r.Write([]byte("123456789€xyz\n"))
	got := tailString(r)
	if !strings.HasPrefix(got, "123456789\x18") {
		t.Errorf("tail = %q, want the head cut before the split rune", got)
	}
	if !strings.Contains(got, "truncated 7 bytes") { // "€xyz\n"
		t.Errorf("tail = %q, want 7 truncated bytes", got)
	}
}

func TestRingRuneSafeCut(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want int
	}{
		{"abcdef", 3, 3},
		{"ab€", 3, 2}, // cut after 1 of €'s 3 bytes
		{"ab€", 4, 2}, // after 2 of 3
		{"ab€", 5, 5}, // the whole rune
		{"abc", 0, 0},
		{"\xff\xfe\xfd", 2, 2}, // invalid bytes are never "partial"
	} {
		if got := runeSafeCut([]byte(tc.in), tc.n); got != tc.want {
			t.Errorf("runeSafeCut(%q, %d) = %d, want %d", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestRingPartialLine: the line being written is part of the tail, is bounded
// by the per-line cap while it grows, and does not pin its peak size once it
// completes — the old ring kept the capacity of the largest partial ever seen.
func TestRingPartialLine(t *testing.T) {
	r := newRing(RingLimits{})
	r.Write([]byte("done\r\n"))
	for range 128 { // 4 MiB with no newline yet
		r.Write(bytes.Repeat([]byte("p"), 32<<10))
	}
	if got := tailString(r); got != "done\r\n"+strings.Repeat("p", DefaultRingLineBytes) {
		t.Fatalf("mid-line tail is %d bytes, want the done line plus the kept head", len(got))
	}
	if got := r.allocated(); got > 128<<10 {
		t.Errorf("an unfinished 4 MiB line holds %d bytes of storage", got)
	}
	r.Write([]byte("\r\n"))
	if got := tailString(r); !strings.HasSuffix(got, fmt.Sprintf("truncated %d bytes]\r\n", 4<<20+2-DefaultRingLineBytes)) {
		t.Errorf("completed line does not end in its marker: ...%q", got[max(len(got)-60, 0):])
	}
	if r.Lines() != 2 {
		t.Errorf("Lines() = %d, want 2", r.Lines())
	}
}

// TestRingByteBudget: under a steady stream the storage never exceeds the
// budget, the tail starts on a line boundary, and it ends at the newest line.
func TestRingByteBudget(t *testing.T) {
	lim := RingLimits{Bytes: 64 << 10, chunkSize: 4 << 10}
	r := newRing(lim)
	var last string
	for i := range 5000 {
		last = fmt.Sprintf("line %05d %s\r\n", i, strings.Repeat("=", i%300))
		r.Write([]byte(last))
		if got := r.allocated(); got > lim.Bytes {
			t.Fatalf("after line %d: %d bytes of storage, over the %d budget", i, got, lim.Bytes)
		}
	}
	tail := tailString(r)
	if !strings.HasPrefix(tail, "line ") || !strings.HasSuffix(tail, last) {
		t.Errorf("tail does not run from a line start to the newest line: %q ... %q",
			tail[:min(len(tail), 20)], tail[max(len(tail)-20, 0):])
	}
	if n := strings.Count(tail, "\n"); n != r.Lines() {
		t.Errorf("tail holds %d lines, Lines() = %d", n, r.Lines())
	}
}

// TestRingNoSteadyStateAllocs: with nobody attached, a full ring recycles its
// oldest chunk, so writing costs no allocation at all.
func TestRingNoSteadyStateAllocs(t *testing.T) {
	r := newRing(RingLimits{})
	line := benchLine(30 << 10)
	for range 200 {
		r.Write(line)
	}
	// 100 writes per run: AllocsPerRun truncates to a whole number per run,
	// so one allocation every other write would still read as zero.
	if avg := testing.AllocsPerRun(20, func() {
		for range 100 {
			r.Write(line)
		}
	}); avg != 0 {
		t.Errorf("100 writes of a 30 KB line allocate %.0f times on a full ring, want 0", avg)
	}
}

// TestRingLentFramesStayValid: a tail frame is a view into storage, not a copy,
// so it must survive the ring moving on — its chunk evicted, and the writer
// looking for a chunk to reuse.
func TestRingLentFramesStayValid(t *testing.T) {
	r := newRing(RingLimits{Bytes: 16 << 10, chunkSize: 1 << 10})
	for i := range 100 {
		r.Write([]byte(fmt.Sprintf("old %03d\n", i)))
	}
	frames := r.tailFrames()
	want := bytes.Join(frames, nil)
	for i := range 20000 { // many budgets' worth, so every lent chunk is evicted
		r.Write([]byte(fmt.Sprintf("new %05d\n", i)))
	}
	if got := bytes.Join(frames, nil); !bytes.Equal(got, want) {
		t.Fatal("a lent tail frame changed after the ring moved on")
	}
	for _, f := range frames {
		if len(f) > r.lim.chunkSize || cap(f) != len(f) {
			t.Fatalf("frame len %d cap %d: want len <= chunk %d and cap == len so an append cannot write into the ring",
				len(f), cap(f), r.lim.chunkSize)
		}
	}
}

// TestRingTailFrameLimit: a budget of more chunks than a queue can take
// replays only its newest maxTailFrames chunks, starting on a line boundary.
func TestRingTailFrameLimit(t *testing.T) {
	r := newRing(RingLimits{Lines: 1 << 20, Bytes: 1 << 20, chunkSize: 1 << 10}) // 1024 chunks
	for i := range 50000 {
		r.Write([]byte(fmt.Sprintf("%06d\n", i))) // 7 bytes per line
	}
	frames := r.tailFrames()
	if len(frames) > maxTailFrames {
		t.Fatalf("%d frames, want <= %d", len(frames), maxTailFrames)
	}
	tail := string(bytes.Join(frames, nil))
	if len(tail)%7 != 0 || !strings.HasSuffix(tail, "049999\n") {
		t.Errorf("limited tail is not whole lines ending at the newest: %d bytes, ends %q",
			len(tail), tail[max(len(tail)-8, 0):])
	}
	if len(tail) < (maxTailFrames-2)*r.lim.chunkSize {
		t.Errorf("limited tail is %d bytes; want close to %d frames' worth", len(tail), maxTailFrames)
	}
}

// refRing is FuzzRing's model: the same per-line truncation applied to a flat
// byte slice, with no chunks, offsets or eviction.
type refRing struct {
	lineBytes int
	out       []byte
	lineStart int
	dropped   int
}

func (f *refRing) write(p []byte) {
	for len(p) > 0 {
		seg, rest, complete := p, []byte(nil), false
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			seg, rest, complete = p[:i+1], p[i+1:], true
		}
		p = rest
		if f.dropped == 0 {
			if room := f.lineBytes - (len(f.out) - f.lineStart); len(seg) <= room {
				f.out = append(f.out, seg...)
			} else {
				keep := runeSafeCut(seg, room)
				f.out = append(f.out, seg[:keep]...)
				f.dropped = len(seg) - keep
			}
		} else {
			f.dropped += len(seg)
		}
		if complete {
			if f.dropped > 0 {
				f.out = truncMarker(f.out, f.dropped)
				f.dropped = 0
			}
			f.lineStart = len(f.out)
		}
	}
}

// FuzzRing drives the ring and the flat model with the same writes and checks
// that the ring's tail is always a line-aligned suffix of the model's output,
// within every limit, without over-evicting, and that frames already handed
// out never change.
func FuzzRing(f *testing.F) {
	f.Add([]byte("hello\nworld\npartial"), uint8(3), uint16(64), uint8(8), uint8(4), uint8(5))
	f.Add([]byte("0123456789abcdef0123456789\nx\ny\n"), uint8(15), uint16(40), uint8(10), uint8(3), uint8(7))
	f.Add([]byte("a\nb\nc\nd\ne\nf\ng\nh\n"), uint8(1), uint16(9), uint8(9), uint8(3), uint8(2))
	f.Add([]byte("ab€cd€ef\n\x1b]8;;http://x\x07link\n"), uint8(9), uint16(200), uint8(6), uint8(5), uint8(3))
	f.Fuzz(func(t *testing.T, data []byte, lines uint8, budget uint16, lineBytes uint8, chunk uint8, split uint8) {
		lim := RingLimits{
			Lines:     int(lines%16) + 1,
			Bytes:     int(budget%4096) + 1,
			LineBytes: int(lineBytes%64) + 1,
			chunkSize: int(chunk%32) + 1,
		}
		r := newRing(lim)
		ref := &refRing{lineBytes: lim.LineBytes}
		step := int(split%16) + 1

		var lent [][]byte
		var lentCopy []byte
		for off := 0; off < len(data); off += step {
			p := data[off:min(off+step, len(data))]
			r.Write(p)
			ref.write(p)

			if lent != nil && !bytes.Equal(bytes.Join(lent, nil), lentCopy) {
				t.Fatal("a lent tail frame changed after later writes")
			}
			frames := r.tailFrames()
			if len(frames) > maxTailFrames {
				t.Fatalf("%d frames, want <= %d", len(frames), maxTailFrames)
			}
			for _, fr := range frames {
				if len(fr) == 0 || len(fr) > lim.chunkSize {
					t.Fatalf("frame of %d bytes; chunk is %d", len(fr), lim.chunkSize)
				}
			}
			tail := bytes.Join(frames, nil)
			lent, lentCopy = frames, tail

			if !bytes.HasSuffix(ref.out, tail) {
				t.Fatalf("tail %q is not a suffix of the model's %q", tail, ref.out)
			}
			if cut := len(ref.out) - len(tail); cut > 0 && ref.out[cut-1] != '\n' {
				t.Fatalf("tail starts mid-line: model %q, tail %q", ref.out, tail)
			}
			n := r.Lines()
			if n > lim.Lines {
				t.Fatalf("Lines() = %d over the cap %d", n, lim.Lines)
			}
			if len(tail) == r.Len() && bytes.Count(tail, []byte("\n")) != n {
				t.Fatalf("tail holds %d lines, Lines() = %d", bytes.Count(tail, []byte("\n")), n)
			}
			// Storage: the budget, or what the current partial line pins
			// when it alone outgrows it (bounded by the line cap).
			partialChunks := (lim.LineBytes+64)/lim.chunkSize + 2
			if limit := (max(r.maxChunks, partialChunks) + 1) * lim.chunkSize; r.allocated() > limit {
				t.Fatalf("%d bytes of storage, over %d", r.allocated(), limit)
			}
			// No over-eviction: below the line cap, only the byte budget
			// evicts, and it stops within a few chunks of the budget.
			slack := ((lim.LineBytes+64)/lim.chunkSize + 4) * lim.chunkSize
			if n < lim.Lines && len(tail) == r.Len() && r.Len() < min(len(ref.out), lim.Bytes-slack) {
				t.Fatalf("retained %d bytes of %d written with a %d budget: over-evicted",
					r.Len(), len(ref.out), lim.Bytes)
			}
		}
	})
}
