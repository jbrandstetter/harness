package diag

// Memory Limit Tests
//
// The property is the runtime's limit, not a value this package computed: each
// application is read back with debug.SetMemoryLimit(-1). These tests change a
// process-wide setting, so none runs in parallel and each restores it.
//
// GOMEMLIMIT is read by the runtime once, before main, so a test cannot set it
// and watch the runtime apply it. The GOMEMLIMIT cases instead put the runtime
// in the state it would have left (a limit already set) and hand
// ResolveMemoryLimit an environment that has the variable.
//
// Governing: SPEC-0010 REQ "Go Memory Limit"; SPEC-0013 REQ-7.
//
// @joestump-agent 09/28/2026 - Added with memlimit.go.

import (
	"math"
	"runtime/debug"
	"testing"
)

// keepMemoryLimit restores the process's limit after the test.
func keepMemoryLimit(t *testing.T) {
	t.Helper()
	was := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(was) })
}

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestResolveMemoryLimitPrecedence(t *testing.T) {
	keepMemoryLimit(t)
	const none = math.MaxInt64 // the runtime's "no limit"
	withGOMEMLIMIT := env(map[string]string{"GOMEMLIMIT": "1GiB"})

	tests := []struct {
		name       string
		runtime    int64 // the limit the runtime started with
		configured int64
		explicit   bool
		source     string
		getenv     func(string) string
		want       MemoryLimit
	}{
		{"config wins over GOMEMLIMIT", 1 << 30, 2 << 30, true, "file", withGOMEMLIMIT,
			MemoryLimit{Bytes: 2 << 30, Source: "file", Explicit: true}},
		{"HARNESS_MEMORY_LIMIT wins over GOMEMLIMIT", 1 << 30, 512 << 20, true, "env", withGOMEMLIMIT,
			MemoryLimit{Bytes: 512 << 20, Source: "env", Explicit: true}},
		{"explicit 0 turns off a GOMEMLIMIT limit", 1 << 30, 0, true, "flag", withGOMEMLIMIT,
			MemoryLimit{Bytes: 0, Source: "flag", Explicit: true}},
		{"GOMEMLIMIT wins over the default", 1 << 30, 0, false, "default", withGOMEMLIMIT,
			MemoryLimit{Bytes: 1 << 30, Source: SourceGOMEMLIMIT}},
		{"GOMEMLIMIT=off reads back as off", none, 0, false, "default", env(map[string]string{"GOMEMLIMIT": "off"}),
			MemoryLimit{Bytes: 0, Source: SourceGOMEMLIMIT}},
		{"unset everywhere is off", none, 0, false, "default", env(nil),
			MemoryLimit{Source: SourceDefault}},
		{"blank GOMEMLIMIT is unset", none, 0, false, "default", env(map[string]string{"GOMEMLIMIT": "  "}),
			MemoryLimit{Source: SourceDefault}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			debug.SetMemoryLimit(tc.runtime)
			got := ResolveMemoryLimit(tc.configured, tc.explicit, tc.source, tc.getenv)
			if got != tc.want {
				t.Errorf("ResolveMemoryLimit = %+v, want %+v", got, tc.want)
			}
			if rt := debug.SetMemoryLimit(-1); rt != tc.runtime {
				t.Errorf("resolving changed the runtime limit: %d, want %d", rt, tc.runtime)
			}
		})
	}
}

// The limit reaches the runtime: read back, not assumed.
func TestApplyMemoryLimitReachesTheRuntime(t *testing.T) {
	keepMemoryLimit(t)
	debug.SetMemoryLimit(math.MaxInt64)

	if got := ApplyMemoryLimit(MemoryLimit{Bytes: 2 << 30, Source: "file", Explicit: true}); got != 2<<30 {
		t.Errorf("ApplyMemoryLimit(2GiB) returned %d", got)
	}
	if rt := debug.SetMemoryLimit(-1); rt != 2<<30 {
		t.Fatalf("runtime limit after applying 2GiB = %d, want %d", rt, int64(2<<30))
	}

	// Not explicit: GOMEMLIMIT's or the runtime's own limit is left alone.
	if got := ApplyMemoryLimit(MemoryLimit{Bytes: 1 << 30, Source: SourceGOMEMLIMIT}); got != 2<<30 {
		t.Errorf("a non-explicit limit changed the runtime: now %d", got)
	}
	if got := ApplyMemoryLimit(MemoryLimit{Source: SourceDefault}); got != 2<<30 {
		t.Errorf("the default changed the runtime: now %d", got)
	}

	// An explicit 0 removes the limit, whoever set it.
	if got := ApplyMemoryLimit(MemoryLimit{Bytes: 0, Source: "env", Explicit: true}); got != 0 {
		t.Errorf("ApplyMemoryLimit(0) returned %d, want 0 (off)", got)
	}
	if rt := debug.SetMemoryLimit(-1); rt != math.MaxInt64 {
		t.Errorf("runtime limit after an explicit 0 = %d, want MaxInt64 (none)", rt)
	}
}
