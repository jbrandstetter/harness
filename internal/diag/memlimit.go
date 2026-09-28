// Package diag holds the daemon's memory guardrails and self-diagnostics: its
// Go soft memory limit, and the opt-in net/http/pprof listener that lets an
// operator see what is using the memory.
//
// # Memory limit
//
// In September 2026 a daemon on a workstation reached 12 GB. Every spawn
// leaked an x/vt emulator of about 98 MiB, and with the default GOGC=100 the
// process footprint runs at about twice the live heap. The fix for the leak
// is in the supervisor. This file is defence in depth: a GC target that keeps
// the heap from ballooning so far past its live set before anyone notices
// (GitHub https://github.com/stump-wtf/harness/issues/18).
//
// It is a soft limit (runtime/debug.SetMemoryLimit), and that shapes every
// decision here. It trims the headroom the GC leaves above the live heap. It
// cannot free a live leak, and near the limit the GC runs back to back, capped
// at about half the CPU, so a limit set below the live heap trades a memory
// problem for a CPU one. The Go GC guide (https://go.dev/doc/gc-guide) warns
// against relying on it where you do not control the environment, and a
// supervisor runs everywhere from a laptop to a 2 GB VM. So it is off unless
// an operator sets it, and the hard cap stays with the init system (systemd
// MemoryMax=, a container limit). memory_limit is set a little below that cap,
// so the GC works harder before the kernel's OOM killer acts.
//
// Precedence, highest first (ResolveMemoryLimit):
//
//  1. --memory-limit, HARNESS_MEMORY_LIMIT, [daemon] memory_limit: the
//     SPEC-0010 ladder, unchanged. An explicit "0" turns the limit off.
//  2. GOMEMLIMIT. The runtime applied it before main ran, so it is already in
//     effect; the daemon reads it back and leaves it alone. It ranks below the
//     harness settings because it is not addressed to this daemon: it is a
//     Go-wide variable that a shell profile or a platform may set, and the
//     daemon passes its environment to every harness it spawns (Go agents
//     read it too). A setting named for the daemon is the more specific
//     statement of intent.
//  3. Off.
//
// Governing: SPEC-0013 REQ-7 (runtime memory visibility), SPEC-0010 REQ
// "Precedence Order", REQ "Go Memory Limit".
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.
package diag

import (
	"math"
	"runtime/debug"
	"strings"
)

// Memory limit sources beyond the settings ladder's flag, env and file.
const (
	// SourceGOMEMLIMIT: no harness setting was given and the runtime is
	// honouring the GOMEMLIMIT environment variable.
	SourceGOMEMLIMIT = "GOMEMLIMIT"
	// SourceDefault: nothing set a limit; it is off.
	SourceDefault = "default"
)

// SmallMemoryLimit is the size below which a limit is almost certainly a
// mistake. A bare number is bytes, so memory_limit = 2048 meant as MiB is a
// 2 KiB limit, and the GC would run continuously. The daemon warns rather than
// refuses: the value is legal, and a tiny limit degrades rather than breaks.
const SmallMemoryLimit = 64 << 20

// MemoryLimit is the resolved limit and where it came from.
type MemoryLimit struct {
	// Bytes is the limit; 0 means none.
	Bytes int64
	// Source is "flag", "env" or "file" for a harness setting, else
	// SourceGOMEMLIMIT or SourceDefault.
	Source string
	// Explicit reports a harness setting. Only an explicit limit is applied;
	// the GOMEMLIMIT and default cases leave the runtime as it started.
	Explicit bool
}

// ResolveMemoryLimit ranks a harness setting over GOMEMLIMIT over off (see
// the package comment). explicit reports that the settings ladder found a
// flag, HARNESS_MEMORY_LIMIT or [daemon] memory_limit, with source naming
// which; configured is its value in bytes, 0 for off. getenv reads the
// process environment.
func ResolveMemoryLimit(configured int64, explicit bool, source string, getenv func(string) string) MemoryLimit {
	if explicit {
		return MemoryLimit{Bytes: configured, Source: source, Explicit: true}
	}
	if strings.TrimSpace(getenv("GOMEMLIMIT")) != "" {
		// The runtime parsed and applied it at startup (and refuses to start
		// on a malformed one), so the value in effect is the authority, not a
		// second parse of the string.
		return MemoryLimit{Bytes: fromRuntime(CurrentMemoryLimit()), Source: SourceGOMEMLIMIT}
	}
	return MemoryLimit{Source: SourceDefault}
}

// ApplyMemoryLimit sets an explicit limit (0 removes any limit, including one
// GOMEMLIMIT set) and returns the limit in effect afterwards, read back from
// the runtime, 0 for none. A limit that is not explicit is left as the
// runtime has it.
func ApplyMemoryLimit(l MemoryLimit) int64 {
	if l.Explicit {
		debug.SetMemoryLimit(toRuntime(l.Bytes))
	}
	return fromRuntime(CurrentMemoryLimit())
}

// CurrentMemoryLimit reads the runtime's limit without changing it
// (math.MaxInt64 means none).
func CurrentMemoryLimit() int64 { return debug.SetMemoryLimit(-1) }

// toRuntime maps "0 is off" onto the runtime's "MaxInt64 is off".
func toRuntime(n int64) int64 {
	if n <= 0 {
		return math.MaxInt64
	}
	return n
}

// fromRuntime maps the runtime's "MaxInt64 is off" onto "0 is off".
func fromRuntime(n int64) int64 {
	if n == math.MaxInt64 {
		return 0
	}
	return n
}
