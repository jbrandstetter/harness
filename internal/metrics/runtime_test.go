package metrics

// Runtime Memory Metrics Tests
//
// Governing tests: SPEC-0013 REQ-7 (the runtime memory series are on the
// scrape) and REQ-8 (pprof never rides the metrics listener). Both read the
// exposition or the HTTP surface a scraper sees.
//
// @joestump-agent 09/28/2026 - Added for GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"math"
	"net/http"
	"runtime/debug"
	"strings"
	"testing"
)

// The memory classes, the GC goal and the leak-signature series are on the
// scrape, each a single unlabelled series.
func TestRuntimeMemoryMetricsExported(t *testing.T) {
	fams := scrape(t, newTestMetrics(t, newFakeSource(), Options{}))

	for _, name := range []string{
		// runtime/metrics /memory/classes/.*, added by runtimeMemoryMetrics.
		"go_memory_classes_heap_objects_bytes",
		"go_memory_classes_heap_free_bytes",
		"go_memory_classes_heap_released_bytes",
		"go_memory_classes_heap_unused_bytes",
		"go_memory_classes_heap_stacks_bytes",
		"go_memory_classes_total_bytes",
		// runtime/metrics /gc/heap/goal:bytes, added by runtimeMemoryMetrics.
		"go_gc_heap_goal_bytes",
		// Collector defaults the docs name as the leak signature and the
		// limit in effect; pinned so a client_golang bump cannot drop them.
		"go_goroutines",
		"go_memstats_heap_objects",
		"go_gc_gomemlimit_bytes",
	} {
		// Free and released heap are legitimately 0 in a young process: the
		// runtime reports 0 for both until its first heap sweep, and a
		// freshly started test process has not had one. Only their presence
		// is asserted. Every other series here is non-zero in any running Go
		// program.
		v := fams.must(t, name, nil)
		if v <= 0 && name != "go_memory_classes_heap_free_bytes" && name != "go_memory_classes_heap_released_bytes" {
			t.Errorf("%s = %v, want > 0", name, v)
		}
	}

	// Cardinality (REQ-5): the added families are a fixed, small set with no
	// labels.
	var classes int
	for name, fam := range fams {
		if !strings.HasPrefix(name, "go_memory_classes_") && name != "go_gc_heap_goal_bytes" {
			continue
		}
		classes++
		for _, m := range fam.GetMetric() {
			if len(m.GetLabel()) != 0 {
				t.Errorf("%s carries labels %v; the runtime families are unlabelled", name, m.GetLabel())
			}
		}
		if n := len(fam.GetMetric()); n != 1 {
			t.Errorf("%s has %d series, want 1", name, n)
		}
	}
	if classes > 20 {
		t.Errorf("%d runtime memory families; the rule matches more of runtime/metrics than intended", classes)
	}
}

// go_gc_gomemlimit_bytes is the limit the runtime holds, so an operator can
// read whether memory_limit took effect from the scrape.
func TestMemoryLimitVisibleOnScrape(t *testing.T) {
	was := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(was) })
	m := newTestMetrics(t, newFakeSource(), Options{})

	debug.SetMemoryLimit(3 << 30)
	if v := scrape(t, m).must(t, "go_gc_gomemlimit_bytes", nil); v != 3<<30 {
		t.Errorf("go_gc_gomemlimit_bytes = %v with a 3GiB limit, want %v", v, float64(3<<30))
	}
	debug.SetMemoryLimit(math.MaxInt64)
	if v := scrape(t, m).must(t, "go_gc_gomemlimit_bytes", nil); v != math.MaxInt64 {
		t.Errorf("go_gc_gomemlimit_bytes = %v with no limit, want MaxInt64", v)
	}
}

// Importing net/http/pprof (through internal/diag) registers its handlers on
// http.DefaultServeMux. The metrics listener serves its own mux, so none of
// them may answer here: pprof is served only on pprof_addr, loopback only
// (REQ-8).
func TestMetricsListenerHasNoPprof(t *testing.T) {
	m := newTestMetrics(t, newFakeSource(), Options{})
	srv, err := Listen(Listener{Addr: "127.0.0.1:0"}, m.Handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/cmdline"} {
		if resp, _ := get(t, "http://"+srv.Addr()+path, ""); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s on the metrics listener = %d, want 404", path, resp.StatusCode)
		}
	}
}
