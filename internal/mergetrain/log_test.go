package mergetrain

// Log Event Documentation Tests
//
// Governing tests: SPEC-0025 REQ-14 — the event list the spec and the usage
// doc publish is exactly the set of events the train emits. Both lists named a
// `ci` event nothing ever logged, and neither named `shutting down`, which the
// driver does log; an operator grepping the daemon log for either was misled.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	eventCall = regexp.MustCompile(`\bevent\("([^"]+)"\)`)
	backticks = regexp.MustCompile("`([^`]+)`")
)

// emittedEvents is every event name passed to event() in the package's
// non-test source. Every call site passes a literal, so a scan of the source
// is the whole set.
func emittedEvents(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range eventCall.FindAllStringSubmatch(string(src), -1) {
			if !slices.Contains(out, m[1]) {
				out = append(out, m[1])
			}
		}
	}
	slices.Sort(out)
	return out
}

// documentedEvents is the event list in the paragraph of doc that introduces
// "`mergetrain <event>`": every backticked name after the last "`err`" key.
func documentedEvents(t *testing.T, doc string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", doc))
	if err != nil {
		t.Fatal(err)
	}
	for _, para := range strings.Split(string(data), "\n\n") {
		if !strings.Contains(para, "`mergetrain <event>`") {
			continue
		}
		i := strings.LastIndex(para, "`err`")
		if i < 0 {
			t.Fatalf("%s: the event paragraph no longer lists the `err` key this test anchors on", doc)
		}
		var out []string
		for _, m := range backticks.FindAllStringSubmatch(para[i+len("`err`"):], -1) {
			out = append(out, m[1])
		}
		slices.Sort(out)
		return out
	}
	t.Fatalf("%s: no paragraph introduces `mergetrain <event>`", doc)
	return nil
}

func TestDocumentedEventsMatchEmitted(t *testing.T) {
	emitted := emittedEvents(t)
	// Guard the scan itself: an empty or tiny set would make every doc
	// comparison meaningless.
	if len(emitted) < 10 || !slices.Contains(emitted, "merged") {
		t.Fatalf("scanned events = %v; the source scan is broken", emitted)
	}
	for _, doc := range []string{
		"docs/usage/merge-train.md",
		"docs/openspec/specs/merge-train/spec.md",
	} {
		documented := documentedEvents(t, doc)
		for _, e := range documented {
			if !slices.Contains(emitted, e) {
				t.Errorf("%s documents event %q, which the train never logs", doc, e)
			}
		}
		for _, e := range emitted {
			if !slices.Contains(documented, e) {
				t.Errorf("%s omits event %q, which the train logs", doc, e)
			}
		}
	}
}
