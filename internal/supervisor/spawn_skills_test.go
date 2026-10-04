package supervisor

// Spawn-Time Skill Projection
//
// projectSkills runs on the single exec path every start, restart,
// crash-restart and triggered firing reaches (SPEC-0006 REQ "Spawn-Time
// Projection"). These tests pin: the merged set lands in the adapter's
// target before exec; a local zero scalar or defaults switch behaves; an
// unreadable root warns without failing the start; a target that cannot be
// written fails naming adapter, target and cause; the skill-repo serving
// root is excluded unconditionally; and an adapter with no target projects
// nothing.
//
// Governing: SPEC-0006 REQ "Spawn-Time Projection", "Ordered Merge and
// Shadowing", "Error Handling Standards"; ADR-0011.
//
// @joestump-agent 10/04/2026 - Added for harness#75.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/skillmerge"
)

func skillHarness(t *testing.T, workdir, skillPaths string) core.Harness {
	t.Helper()
	h := core.Harness{
		Name:                 "reviewer",
		Adapter:              "claude-code",
		Workdir:              workdir,
		UseDefaultSkillPaths: true,
	}
	for _, p := range strings.Split(skillPaths, ",") {
		if p = strings.TrimSpace(p); p != "" {
			h.SkillPaths = append(h.SkillPaths, p)
		}
	}
	return h
}

func writeSkillDir(t *testing.T, root, slug, body string) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, skillmerge.SkillSlug), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The merged set lands in the adapter's target: a workdir skill and a
// skill_paths skill both project, the target root is excluded from the
// merge, and the projection is by copy.
func TestProjectSkillsMergesIntoTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".claude", "skills")

	workdir := t.TempDir()
	writeSkillDir(t, filepath.Join(workdir, ".claude", "skills"), "workdir-skill", "from workdir")
	paths := t.TempDir()
	writeSkillDir(t, paths, "path-skill", "from skill_paths")

	h := skillHarness(t, workdir, paths)
	if err := projectSkills(h, workdir); err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]string{
		"workdir-skill": "from workdir",
		"path-skill":    "from skill_paths",
	} {
		raw, err := os.ReadFile(filepath.Join(target, slug, skillmerge.SkillSlug))
		if err != nil {
			t.Fatalf("projected %s missing: %v", slug, err)
		}
		if string(raw) != want {
			t.Fatalf("%s projected wrong content: %q", slug, raw)
		}
	}
}

// A collision resolves to the higher tier: skill_paths shadows the adapter
// default for the same slug.
func TestProjectSkillsSkillPathsShadowDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := t.TempDir()
	writeSkillDir(t, filepath.Join(workdir, ".claude", "skills"), "playbook", "from workdir default")
	paths := t.TempDir()
	writeSkillDir(t, paths, "playbook", "from skill_paths")

	h := skillHarness(t, workdir, paths)
	if err := projectSkills(h, workdir); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "playbook", skillmerge.SkillSlug))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "from skill_paths" {
		t.Fatalf("skill_paths must shadow the adapter default, got %q", raw)
	}
}

// use_default_skill_paths = false drops the adapter's default roots.
func TestProjectSkillsDefaultsDroppable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := t.TempDir()
	writeSkillDir(t, filepath.Join(workdir, ".claude", "skills"), "workdir-skill", "from workdir default")
	paths := t.TempDir()
	writeSkillDir(t, paths, "path-skill", "from skill_paths")

	h := skillHarness(t, workdir, paths)
	h.UseDefaultSkillPaths = false
	if err := projectSkills(h, workdir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "workdir-skill")); !os.IsNotExist(err) {
		t.Fatal("the dropped default root must not project")
	}
	raw, err := os.ReadFile(filepath.Join(home, ".claude", "skills", "path-skill", skillmerge.SkillSlug))
	if err != nil || string(raw) != "from skill_paths" {
		t.Fatalf("the configured root must still project: %v", err)
	}
}

// An unreadable root warns and the start proceeds with the rest merged.
// The blocked root puts a regular file in the path's way: a CI runner
// executing as root bypasses mode bits, so a chmod 000 directory reads
// fine there and the tolerance would go untested (ENOTDIR is denied to
// root too).
func TestProjectSkillsUnreadableRootWarns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := t.TempDir()
	paths := t.TempDir()
	writeSkillDir(t, paths, "path-skill", "from skill_paths")
	blockedParent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blockedParent, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(blockedParent, "locked")

	h := skillHarness(t, workdir, strings.Join([]string{blocked, paths}, ", "))
	if err := projectSkills(h, workdir); err != nil {
		t.Fatalf("an unreadable root must not block the start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "path-skill")); err != nil {
		t.Fatalf("the remaining roots must still merge: %v", err)
	}
}

// A target that cannot be written fails the start naming adapter, target
// and cause, wrapped around the projection sentinel.
func TestProjectSkillsUnwritableTargetFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	workdir := t.TempDir()
	writeSkillDir(t, workdir, "workdir-skill", "x")
	// A FILE where ~/.claude/skills must be a directory.
	if err := os.MkdirAll(filepath.Dir(filepath.Join(home, ".claude", "skills")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := skillHarness(t, workdir, "")
	err := projectSkills(h, workdir)
	if !errors.Is(err, skillmerge.ErrTargetNotWritable) {
		t.Fatalf("want the projection sentinel, got %v", err)
	}
	if !strings.Contains(err.Error(), "claude-code") || !strings.Contains(err.Error(), filepath.Join(home, ".claude", "skills")) {
		t.Fatalf("the error must name the adapter and the target: %v", err)
	}
}

// A harness on an adapter with no target projects nothing and starts
// normally.
func TestProjectSkillsNoTargetSkips(t *testing.T) {
	h := core.Harness{Name: "scratch", Adapter: "generic", SkillPaths: []string{t.TempDir()}}
	if err := projectSkills(h, t.TempDir()); err != nil {
		t.Fatalf("a no-target adapter must skip projection: %v", err)
	}
}

// The SPEC-0007 serving store is excluded from projection unconditionally,
// even when an operator points skill_paths into it.
func TestProjectSkillsExcludesServingStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	serving := filepath.Join(state, "harness", "skills", "team", "skills")
	writeSkillDir(t, serving, "repo-skill", "from a skill repo")

	workdir := t.TempDir()
	writeSkillDir(t, filepath.Join(workdir, ".claude", "skills"), "workdir-skill", "from workdir")

	h := skillHarness(t, workdir, serving)
	if err := projectSkills(h, workdir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "repo-skill")); !os.IsNotExist(err) {
		t.Fatal("a skill-repo skill must never project")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "workdir-skill")); err != nil {
		t.Fatalf("the other roots must still project: %v", err)
	}

	// The store directory itself is equally out of bounds: a skill_paths
	// entry equal to the store root must not project it.
	//
	// @joestump-agent 10/04/2026 - Added with the store-root exclusion for
	// harness#893.
	storeRoot := filepath.Join(state, "harness", "skills")
	writeSkillDir(t, storeRoot, "store-root-skill", "from the store root")
	hStore := skillHarness(t, workdir, storeRoot)
	if err := projectSkills(hStore, workdir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "store-root-skill")); !os.IsNotExist(err) {
		t.Fatal("the serving store root itself must never project")
	}
}

// The package bundle tier (SPEC-0026 REQ-10): a sourced harness whose
// pinned manifest requested skill_paths contributes the pin's bundled
// skills at the lowest tier — skill_paths shadows it, and only pinned
// content projects (a post-install edit to the stable's clone changes
// nothing).
//
// Governing: SPEC-0026 REQ-10; SPEC-0006 REQ "Ordered Merge and Shadowing".
//
// @joestump-agent 10/04/2026 - Added for harness#816.
func TestProjectSkillsPackageBundleTier(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	// The store is deliberately read-only; restore write permission so the
	// temp dir's cleanup can remove it.
	t.Cleanup(func() {
		_ = agentpkg.ChmodTreeWritable(agentpkg.StateHome())
		_ = os.RemoveAll(agentpkg.StateHome())
	})

	workdir := t.TempDir()
	writeSkillDir(t, filepath.Join(workdir, ".claude", "skills"), "playbook", "operator copy")
	paths := t.TempDir()
	writeSkillDir(t, paths, "path-only", "from skill_paths")

	manifest := `[package]
name = "pr-reviewer"

[harness]
harness = "claude-code"

[requests]
skill_paths = true
`
	// The fixture remote: manifest requesting the bundle, plus two skills.
	work := t.TempDir()
	gitRunT(t, work, "init", "-q", "-b", "main")
	writeSkillDir(t, filepath.Join(work, "packages/pr-reviewer", "skills"), "playbook", "bundled")
	writeSkillDir(t, filepath.Join(work, "packages/pr-reviewer", "skills"), "bundled-only", "bundled only")
	if err := os.MkdirAll(filepath.Join(work, "packages/pr-reviewer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRunT(t, work, "add", "-A")
	gitRunT(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "fixture")
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRunT(t, work, "clone", "-q", "--bare", work, remote)

	if _, err := agentpkg.CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	src, err := agentpkg.ResolvePin("stump-wtf", "pr-reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := agentpkg.Materialize(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentpkg.Place(src, tmp); err != nil {
		t.Fatal(err)
	}

	// Post-install, the clone's content moves on: the pin must not follow.
	writeSkillDir(t, filepath.Join(agentpkg.StableDir("stump-wtf"), "packages/pr-reviewer"), "playbook", "EDITED IN CLONE")

	h := skillHarness(t, workdir, paths)
	h.PackageSource = src.String()
	target := filepath.Join(home, ".claude", "skills")
	if err := projectSkills(h, workdir); err != nil {
		t.Fatal(err)
	}

	// The operator's global skill shadows the bundle.
	raw, err := os.ReadFile(filepath.Join(target, "playbook", skillmerge.SkillSlug))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "operator copy" {
		t.Fatalf("the operator's skill must win, got %q", raw)
	}
	// Bundled-only skills land at the lowest tier, from the PIN.
	raw, err = os.ReadFile(filepath.Join(target, "bundled-only", skillmerge.SkillSlug))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "bundled only" {
		t.Fatalf("the bundled-only skill must project from the pin: %q", raw)
	}

	// A harness with no source behaves exactly as SPEC-0006 defines.
	hNoSource := skillHarness(t, workdir, paths)
	if err := projectSkills(hNoSource, workdir); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(target, "playbook", skillmerge.SkillSlug))
	if err != nil || string(raw) != "operator copy" {
		t.Fatalf("a sourceless harness must be unchanged: %v %q", err, raw)
	}
}

func gitRunT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
