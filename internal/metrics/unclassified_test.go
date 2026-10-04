package metrics

// Unclassified Control Tests
//
// Governing tests: SPEC-0013 REQ-3; design.md "Testing" — the unclassified
// control, end to end through the collector. The classifier's own tables are
// tested where it lives, internal/modelerr.
//
// @joestump-agent 09/21/2026 - Added for harness#356.
//
// @joestump 10/04/2026 - Split out of classify_test.go when the classifier
// moved to internal/modelerr (harness#473). The test is unchanged but for the
// modelerr qualifier on the classes.

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/modelerr"
)

// The control, end to end: an unrecognised error increments both class=other
// and the unclassified counter; a recognised context-window error increments
// class=other only. Without the second half, every wedged session would read
// as a provider changing its wording.
func TestUnclassifiedErrorIncrementsControl(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})

	feed.ch <- errorEvent("worker", "s1", "Bad Request: something no provider has said before", t0)
	eventually(t, "the error counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "error"))
		return v == 1
	})
	fams := scrape(t, m)
	if v := fams.must(t, "harness_model_call_errors_total", lbls("harness", "worker", "class", "other")); v != 1 {
		t.Errorf("class=other = %v, want 1", v)
	}
	if v := fams.must(t, "harness_model_call_errors_unclassified_total", lbls("harness", "worker")); v != 1 {
		t.Errorf("unclassified = %v, want 1", v)
	}

	feed.ch <- errorEvent("worker", "s1", "Bad Request: litellm.ContextWindowExceededError: prompt contains at least 196609 input tokens", t0.Add(time.Second))
	eventually(t, "the context error counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "error"))
		return v == 2
	})
	fams = scrape(t, m)
	if v := fams.must(t, "harness_model_call_errors_total", lbls("harness", "worker", "class", "other")); v != 2 {
		t.Errorf("class=other = %v, want 2", v)
	}
	if v := fams.must(t, "harness_model_call_errors_unclassified_total", lbls("harness", "worker")); v != 1 {
		t.Errorf("unclassified = %v after a context-window error, want still 1", v)
	}
	for _, c := range []modelerr.Class{modelerr.ClassQuota, modelerr.ClassAuth, modelerr.ClassTimeout, modelerr.ClassTransport} {
		if v := fams.must(t, "harness_model_call_errors_total", lbls("harness", "worker", "class", string(c))); v != 0 {
			t.Errorf("class=%s = %v, want 0", c, v)
		}
	}
}
