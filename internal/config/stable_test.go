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

// SPEC-0006 REQ "Skill Path Configuration": skill_paths resolve against the
// declaring file at load, use_default_skill_paths defaults to true, and a
// project file carrying both is accepted — relative paths resolving against
// the project root.
func TestSkillPathConfiguration(t *testing.T) {
	global := `
[harness.reviewer]
harness = "claude-code"
prompt = "review"
skill_paths = ["~/team-skills", "relative/from-global", ""]
`
	_, err := Parse([]byte(global), "harness.toml")
	if err == nil || !strings.Contains(err.Error(), "empty entries") {
		t.Fatalf("an empty skill_paths entry must be refused, got %v", err)
	}

	// The global file stores paths raw — exactly like workdir and env_file —
	// and spawn expands ~ against the operator's home.
	global = `
[harness.reviewer]
harness = "claude-code"
prompt = "review"
skill_paths = ["~/team-skills", "relative/from-global"]
use_default_skill_paths = false
`
	cfg, err := Parse([]byte(global), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["reviewer"]
	if len(h.SkillPaths) != 2 || h.SkillPaths[0] != "~/team-skills" || h.SkillPaths[1] != "relative/from-global" {
		t.Fatalf("global skill_paths stay raw until spawn, got %v", h.SkillPaths)
	}
	if h.UseDefaultSkillPaths {
		t.Errorf("use_default_skill_paths = false must resolve false")
	}

	// A project file carries both keys, relative against the project root.
	proj := `
[project]
name = "demo"

[harness.reviewer]
harness = "claude-code"
prompt = "review"
skill_paths = ["./skills"]
`
	projPath := filepath.Join(t.TempDir(), "proj", "harness.toml")
	if err := os.MkdirAll(filepath.Dir(projPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projPath, []byte(proj), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProject(projPath)
	if err != nil {
		t.Fatalf("a project file must carry skill_paths: %v", err)
	}
	ph := loaded.Config.Harnesses["reviewer"]
	if len(ph.SkillPaths) != 1 || !strings.Contains(ph.SkillPaths[0], "skills") {
		t.Fatalf("project-relative skill_paths must resolve: %v", ph.SkillPaths)
	}
	if !ph.UseDefaultSkillPaths {
		t.Fatal("unset defaults to true")
	}
}
