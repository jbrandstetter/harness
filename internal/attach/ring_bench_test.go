package attach

// Benchmarks for the byte-bounded ring (ADR-0007) and the chunked attach
// replay (SPEC-0002 REQ "Attach Session"). The PR that introduced them quotes
// these against the line-capped ring they replaced.

import (
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// benchRingWrite measures Write into an already-full default ring.
func benchRingWrite(b *testing.B, lineLen int) {
	r := newRing(RingLimits{})
	line := benchLine(lineLen)
	for range 2 * DefaultRingLines {
		r.Write(line)
	}
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		r.Write(line)
	}
}

// BenchmarkRingWrite30K is a typical fat stream-json line.
func BenchmarkRingWrite30K(b *testing.B) { benchRingWrite(b, 30<<10) }

// BenchmarkRingWrite80B is a shell-width line, where the line cap binds.
func BenchmarkRingWrite80B(b *testing.B) { benchRingWrite(b, 80) }

// benchAttach measures opening and closing a session on a mux that has seen
// 40 MiB of 30 KB lines.
func benchAttach(b *testing.B, lim RingLimits) {
	m := newMuxLimits("h", lim, nil, nil, nil)
	line := benchLine(30 << 10)
	for range (40 << 20) / len(line) {
		m.ring.Write(line)
	}
	m.Write(line) // and give the snapshot a screenful to render
	b.ReportMetric(float64(m.ring.Len()), "ring-bytes")
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := m.Attach(1, protocol.AttachRO, 80, 24, func([]byte) error { return nil })
		m.Detach(s)
	}
}

// BenchmarkAttach40MiB uses the default budget.
func BenchmarkAttach40MiB(b *testing.B) { benchAttach(b, RingLimits{}) }

// BenchmarkAttach40MiBBigRing keeps all 40 MiB, to show a session costs no
// allocation in proportion to the ring however large it is.
func BenchmarkAttach40MiBBigRing(b *testing.B) { benchAttach(b, RingLimits{Bytes: 64 << 20}) }
