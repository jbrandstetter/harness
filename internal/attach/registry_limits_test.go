package attach

// Governing: ADR-0007 (configurable, byte-bounded scrollback ring per harness).

import "testing"

// TestRegistryLimitsReachRing: the limits a Registry is built with are the
// ones every Mux it creates gives its ring, and Limits reports them.
func TestRegistryLimitsReachRing(t *testing.T) {
	reg := NewRegistryLimits(RingLimits{Lines: 7, Bytes: 2 << 20})
	got := reg.Mux("h").ring.lim
	if got != reg.Limits() {
		t.Errorf("ring limits %+v, Registry.Limits() %+v", got, reg.Limits())
	}
	if got.Lines != 7 || got.Bytes != 2<<20 || got.LineBytes != DefaultRingLineBytes {
		t.Errorf("ring limits %+v, want 7 lines, 2 MiB, default line cap", got)
	}

	def := NewRegistry(0).Limits()
	if def.Lines != DefaultRingLines || def.Bytes != DefaultRingBytes {
		t.Errorf("NewRegistry(0).Limits() = %+v, want the defaults", def)
	}
}
