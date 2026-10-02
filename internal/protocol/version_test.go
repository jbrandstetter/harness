package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

// ProtoMinor 22 (issue #815; ADR-0044): PackageKeys rides HarnessInfo,
// additive only — an older peer sees an unknown field and ignores it, a
// newer daemon's data survives an older client's decode untouched.
func TestHarnessInfoPackageKeysRoundTrip(t *testing.T) {
	info := HarnessInfo{
		Name:        "pr-reviewer",
		Source:      "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000",
		PackageKeys: []string{"harness", "model"},
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"package_keys":["harness","model"]`) {
		t.Fatalf("package_keys must marshal compactly on the wire: %s", raw)
	}
	var back HarnessInfo
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.PackageKeys) != 2 || back.PackageKeys[0] != "harness" {
		t.Fatalf("round trip lost the keys: %+v", back)
	}

	// An older client's decode (unknown field) ignores it without error.
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, exists := legacy["package_keys"]; !exists {
		t.Fatal("the field must be on the wire for newer clients")
	}
}
