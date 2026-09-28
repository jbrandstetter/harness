// Serving-clone tests: the default-branch gate, the sync path, and the skill
// file format.
//
// Governing: SPEC-0007 REQ "Default-Branch Gate", REQ "Skill Artifact".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitRun runs a git command in dir and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v: %s", strings.Join(args, " "), dir, err, out)
	}
}

// commitSkillRepo writes skills into dir and commits them, so tests start
// from a clean default-branch checkout.
func commitSkillRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "skills")
}

// newServingClone builds an origin repository with one active skill, clones
// it to a serving-clone path, and returns both directories.
func newServingClone(t *testing.T) (origin, clone string) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin")
	clone = filepath.Join(root, "clone")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, origin, "init", "-q", "-b", "main")
	commitSkillRepo(t, origin, map[string]string{
		"skills/pin-go/SKILL.md": testSkill("pin-go", "active"),
	})
	gitRun(t, origin, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "anchor")
	// A clone records origin/HEAD, which defaultBranch() reads.
	gitRun(t, root, "clone", "-q", origin, clone)
	return origin, clone
}

// testSkill renders a minimal valid skill file in the skill-repo vocabulary
// (status active/retired).
func testSkill(name, status string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"description: Pin the Go toolchain in CI before bumping go.mod.\n" +
		"status: " + status + "\n" +
		"tags:\n  - ci\n" +
		"symptoms:\n  - \"go: cannot find main module\"\n" +
		"applies_to:\n  - \".gitea/workflows/*\"\n" +
		"---\n" +
		"## When to use\n\nA CI run fails.\n\n" +
		"## Steps\n\n1. Pin it.\n\n" +
		"## Invariants\n\n- Never disagree.\n\n" +
		"## Failure modes\n\n- Partial pin.\n\n" +
		"## Do not\n\n- Do not bump blindly.\n\n" +
		"## Evidence\n\n- reduit#191.\n"
}

func TestInspectStates(t *testing.T) {
	_, clone := newServingClone(t)

	info, err := Inspect(clone)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if info.State != CloneOK || info.DefaultBranch != "main" || info.Head != "main" {
		t.Fatalf("want ok/main/main, got %+v", info)
	}

	// Detached HEAD leaves the gate.
	gitRun(t, clone, "checkout", "-q", "--detach", "HEAD")
	info, err = Inspect(clone)
	if err != nil {
		t.Fatalf("inspect detached: %v", err)
	}
	if info.State != CloneDetached || info.Detail == "" {
		t.Fatalf("want detached with detail, got %+v", info)
	}

	// A proposal branch is also off the default branch.
	gitRun(t, clone, "checkout", "-q", "-b", "proposal/pin-go")
	info, err = Inspect(clone)
	if err != nil {
		t.Fatalf("inspect branch: %v", err)
	}
	if info.State != CloneDetached || info.Head != "proposal/pin-go" {
		t.Fatalf("want detached proposal/pin-go, got %+v", info)
	}

	// Back on the default branch but dirty.
	gitRun(t, clone, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(clone, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err = Inspect(clone)
	if err != nil {
		t.Fatalf("inspect dirty: %v", err)
	}
	if info.State != CloneDirty || info.Detail == "" {
		t.Fatalf("want dirty with detail, got %+v", info)
	}

	// No clone at all.
	info, err = Inspect(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("inspect missing: %v", err)
	}
	if info.State != CloneMissing {
		t.Fatalf("want missing, got %+v", info)
	}
}

func TestSyncRepoCreatesAndFastForwards(t *testing.T) {
	origin, _ := newServingClone(t)
	clone := filepath.Join(t.TempDir(), "skills", "go-stack")

	res, err := SyncRepo("go-stack", origin, clone)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !res.FastForwarded || !res.Created || res.DefaultBranch != "main" {
		t.Fatalf("want created fast-forward to main, got %+v", res)
	}

	// Up to date: no fast-forward to report.
	res, err = SyncRepo("go-stack", origin, clone)
	if err != nil {
		t.Fatalf("sync again: %v", err)
	}
	if res.FastForwarded || res.Created {
		t.Fatalf("want no-op, got %+v", res)
	}

	// A new origin commit fast-forwards the clone.
	commitSkillRepo(t, origin, map[string]string{
		"skills/pin-go/SKILL.md": testSkill("pin-go", "active"),
		"skills/pin-ci/SKILL.md": testSkill("pin-ci", "active"),
	})
	res, err = SyncRepo("go-stack", origin, clone)
	if err != nil {
		t.Fatalf("sync after new commit: %v", err)
	}
	if !res.FastForwarded {
		t.Fatalf("want fast-forward, got %+v", res)
	}
	info, err := Inspect(clone)
	if err != nil || info.State != CloneOK {
		t.Fatalf("clone should stay clean on the default branch: %+v %v", info, err)
	}
}

func TestSyncRepoLeavesDetachedCloneAlone(t *testing.T) {
	origin, clone := newServingClone(t)
	gitRun(t, clone, "checkout", "-q", "--detach", "HEAD")

	res, err := SyncRepo("go-stack", origin, clone)
	if err != nil {
		t.Fatalf("sync detached: %v", err)
	}
	if res.FastForwarded || res.Detail == "" {
		t.Fatalf("want no fast-forward with detail, got %+v", res)
	}
	info, err := Inspect(clone)
	if err != nil || info.State != CloneDetached {
		t.Fatalf("clone must stay detached: %+v %v", info, err)
	}
}

func TestParseSkillFileStatuses(t *testing.T) {
	if _, err := ParseSkillFile(testSkill("a", "active")); err != nil {
		t.Fatalf("active should parse: %v", err)
	}
	if _, err := ParseSkillFile(testSkill("a", "retired")); err != nil {
		t.Fatalf("retired should parse: %v", err)
	}
	if _, err := ParseSkillFile(testSkill("a", "promoted")); err == nil {
		t.Fatal("learned-tier status must be outside the skill-repo vocabulary")
	}
	if _, err := ParseSkillFile("no frontmatter"); err == nil {
		t.Fatal("missing frontmatter must fail")
	}
}

func TestCollectEntriesSkipsMalformedAndRetired(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "clone")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, clone, "init", "-q", "-b", "main")
	commitSkillRepo(t, clone, map[string]string{
		"skills/good/SKILL.md": testSkill("good", "active"),
		"skills/old/SKILL.md":  testSkill("old", "retired"),
		"skills/bad/SKILL.md":  "not a skill at all",
	})

	entries, warnings, err := collectEntries("repo", clone, "skills")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(entries) != 1 || entries[0].Slug != "good" || entries[0].Repo != "repo" {
		t.Fatalf("want only the active skill, got %+v", entries)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "skills/bad/SKILL.md") {
		t.Fatalf("want one warning naming the malformed file, got %v", warnings)
	}
}

func TestMergedAt(t *testing.T) {
	_, clone := newServingClone(t)
	secs, err := MergedAt(clone, "skills/pin-go/SKILL.md")
	if err != nil || secs <= 0 {
		t.Fatalf("want a commit time, got %d %v", secs, err)
	}
	if time.Unix(secs, 0).IsZero() {
		t.Fatal("zero time")
	}
}
