package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

func TestParseStables(t *testing.T) {
	cfg, err := Parse([]byte(`
[stable.stump-wtf]
remote = "https://gitea.example/stump-wtf/stable.git"

[stable.internal]
remote = "git@internal.example:stable.git"
public = false
`), "t.toml")
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	got := cfg.OrderedStables()
	if len(got) != 2 {
		t.Fatalf("want 2 stables, got %d", len(got))
	}
	if got[0].Name != "stump-wtf" || got[0].Remote != "https://gitea.example/stump-wtf/stable.git" || !got[0].Public {
		t.Errorf("unset public must resolve true, got %+v", got[0])
	}
	if got[1].Public {
		t.Errorf("explicit public = false must stay false, got %+v", got[1])
	}
}

func TestParseStableRejectsBadTables(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{"bad name", "[stable.Evil]\nremote = \"https://x/y.git\"\n", "name must match"},
		{"missing remote", "[stable.ghost]\npublic = true\n", "remote is required"},
		{"duplicate", "[stable.one]\nremote = \"https://x/1.git\"\n\n[stable.one]\nremote = \"https://x/2.git\"\n", "already been defined"},
		{"unknown key", "[stable.one]\nremote = \"https://x/1.git\"\nbranches = [\"main\"]\n", "unknown key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.src), "t.toml")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("want error containing %q, got %v", tc.wantSub, err)
			}
		})
	}
}

// A project file cannot add a stable (SPEC-0026 REQ-1): the declaration is
// refused naming [stable.*] as global-only, and no clone is ever attempted —
// parsing never touches the network or the stable store.
func TestStableGlobalOnlyInProjectFile(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	_, err := ParseProject([]byte("[stable.evil]\nremote = \"https://attacker.example/stable.git\"\n"), "harness.toml")
	if err == nil {
		t.Fatal("project [stable.evil] must be refused")
	}
	if !strings.Contains(err.Error(), "stable.evil") || !strings.Contains(err.Error(), "global-only") {
		t.Fatalf("error must name [stable.*] as global-only, got %v", err)
	}
	if _, err := os.Stat(agentpkg.StableRoot()); !os.IsNotExist(err) {
		t.Fatalf("no clone directory may appear under %s", agentpkg.StableRoot())
	}
}

// A harness_d drop-in may only carry [harness.*], [channel.*] and
// [webhook.*]; [stable.x] is refused there too (SPEC-0026 REQ-1).
func TestHarnessDRejectsStableTable(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.toml"), []byte(`
[stable.sneaky]
remote = "https://attacker.example/stable.git"
`), 0644); err != nil {
		t.Fatal(err)
	}
	main := fmt.Sprintf("[server]\nharness_d = %q\n", dir)
	_, err := Parse([]byte(main), "t.toml")
	if err == nil {
		t.Fatal("expected error for [stable.*] in harness.d file")
	}
	if !strings.Contains(err.Error(), "must not contain [stable.sneaky]") {
		t.Errorf("error %q does not name the forbidden table", err.Error())
	}
}

func TestNoStableDeclared(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OrderedStables()) != 0 {
		t.Fatal("no stables should be declared")
	}
}
