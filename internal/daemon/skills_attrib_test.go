package daemon

// Describe Skill Attribution
//
// The describe reply carries the harness's resolved skill set with its
// shadow map (SPEC-0006 REQ "Ordered Merge and Shadowing": shadowed copies
// MUST remain enumerable). The attribution is a pure local read over the
// same roots spawn's projection draws from — never a fetch — and is absent
// for a harness with no skill ground.
//
// Governing: SPEC-0006 REQ "Ordered Merge and Shadowing"; ADR-0011.
//
// @joestump-agent 10/04/2026 - Added for harness#75.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/skillmerge"
)

func TestSkillAttributions(t *testing.T) {
	c := &conn{}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	workdir := t.TempDir()
	writeSkillDirT(t, filepath.Join(workdir, ".claude", "skills"), "playbook", "default copy")
	paths := t.TempDir()
	writeSkillDirT(t, paths, "playbook", "operator copy")
	writeSkillDirT(t, paths, "unique", "operator only")

	h := core.Harness{
		Name:                 "reviewer",
		Adapter:              "claude-code",
		Workdir:              workdir,
		SkillPaths:           []string{paths},
		UseDefaultSkillPaths: true,
	}
	out := c.skillAttributions(h)
	if len(out) != 2 {
		t.Fatalf("want 2 attributed skills, got %+v", out)
	}
	byName := map[string]int{}
	for i, sa := range out {
		byName[sa.Name] = i
	}
	pb := out[byName["playbook"]]
	if pb.WinnerSource != "skill_paths" {
		t.Fatalf("skill_paths must shadow the adapter default: %+v", pb)
	}
	if len(pb.Shadowed) != 1 || !strings.Contains(pb.Shadowed[0], "playbook") {
		t.Fatalf("the shadowed default copy must be enumerable: %+v", pb)
	}
	if out[byName["unique"]].WinnerSource != "skill_paths" {
		t.Fatalf("the unique skill must attribute to skill_paths: %+v", out[byName["unique"]])
	}

	// Defaults dropped: the workdir copy no longer contributes.
	h.UseDefaultSkillPaths = false
	out = c.skillAttributions(h)
	if len(out) != 2 {
		t.Fatalf("the default root must contribute nothing when dropped: %+v", out)
	}
	for _, sa := range out {
		if len(sa.Shadowed) != 0 {
			t.Fatalf("no shadowing without the default root: %+v", sa)
		}
	}

	// A harness with no skill ground attributes nothing.
	if got := c.skillAttributions(core.Harness{Name: "plain", Adapter: "command"}); got != nil {
		t.Fatalf("no ground, no attribution: %+v", got)
	}
}

// The package bundle tier (SPEC-0026 REQ-10) attributes in describe: the
// pin's bundled skills contribute at the lowest tier, shadowed by any
// higher-tier copy.
func TestSkillAttributionsPackageBundle(t *testing.T) {
	c := &conn{}
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Cleanup(func() {
		_ = os.RemoveAll(state)
	})

	src := "stump-wtf/pr-reviewer@" + strings.Repeat("a", 40)
	bundle := filepath.Join(state, "harness", "agents", "installed", "stump-wtf", "pr-reviewer", strings.Repeat("a", 40))
	writeSkillDirT(t, filepath.Join(bundle, "skills"), "playbook", "bundled copy")
	writeSkillDirT(t, filepath.Join(bundle, "skills"), "bundled-only", "bundled only")
	manifest := `[package]
name = "pr-reviewer"

[harness]
harness = "claude-code"

[requests]
skill_paths = true
`
	if err := os.WriteFile(filepath.Join(bundle, "package.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	global := t.TempDir()
	writeSkillDirT(t, global, "playbook", "operator copy")

	h := core.Harness{
		Name:                 "reviewer",
		Adapter:              "claude-code",
		PackageSource:        src,
		SkillPaths:           []string{global},
		UseDefaultSkillPaths: true,
	}
	out := c.skillAttributions(h)
	byName := map[string]protocol.SkillAttribution{}
	for _, sa := range out {
		byName[sa.Name] = sa
	}
	pb, ok := byName["playbook"]
	if !ok {
		t.Fatalf("playbook must resolve: %+v", out)
	}
	if !strings.Contains(pb.Winner, global) {
		t.Fatalf("the operator's copy must win: %+v", pb)
	}
	found := false
	for _, sh := range pb.Shadowed {
		if strings.Contains(sh, "installed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the package copy must be recorded shadowed: %+v", pb)
	}
	if bo, ok := byName["bundled-only"]; !ok || !strings.Contains(bo.Winner, "installed") {
		t.Fatalf("the bundled-only skill must attribute to the bundle: %+v", out)
	}

	// The serving store itself stays out of bounds for attribution,
	// exactly as the spawn-time projection excludes it: a skill_paths
	// entry equal to the store root must not attribute.
	//
	// @joestump-agent 10/04/2026 - Added with the store-root exclusion for
	// harness#893.
	storeRoot := filepath.Join(state, "harness", "skills")
	writeSkillDirT(t, storeRoot, "store-root-only", "from the store root")
	hStore := h
	hStore.SkillPaths = append(append([]string{}, h.SkillPaths...), storeRoot)
	for _, sa := range c.skillAttributions(hStore) {
		if sa.Name == "store-root-only" {
			t.Fatalf("the serving store root must not attribute: %+v", sa)
		}
	}
}

func writeSkillDirT(t *testing.T, root, slug, body string) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillmerge.SkillSlug), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
