package agentpkg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/gitcmd"
)

// gitRun runs git, failing the test on error. Every fixture remote is a
// local bare repository on the filesystem — no test touches the network.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitcmd.Run(dir, args...)
	if err != nil {
		t.Fatalf("git %v (in %s): %v: %s", args, dir, err, out)
	}
	return out
}

// newStableRemote builds a bare repository carrying files and returns its
// path, plus the work tree it was built from (for later commits).
func newStableRemote(t *testing.T, files map[string]string) (remote, work string) {
	t.Helper()
	work = t.TempDir()
	gitRun(t, work, "init", "-q", "-b", "main")
	writeFiles(t, work, files)
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "fixture")
	remote = filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, work, "clone", "-q", "--bare", work, remote)
	return remote, work
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func isolatedState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

const validPkg = `[package]
name = "pr-reviewer"
version = "1.0.0"
description = "reviews pull requests"

[harness]
harness = "claude-code"
`

// Adding a stable clones before trusting: a failed clone writes no table and
// leaves no stables/<name>/ directory (and no temp directory) behind.
func TestCloneStableFailedCloneLeavesNothingBehind(t *testing.T) {
	isolatedState(t)

	_, err := CloneStable("ghost", filepath.Join(t.TempDir(), "missing.git"))
	if err == nil {
		t.Fatal("clone from a missing remote must fail")
	}
	if _, err := os.Stat(StableDir("ghost")); !os.IsNotExist(err) {
		t.Fatalf("no stables/ghost/ directory may be left behind, stat: %v", err)
	}
	entries, err := os.ReadDir(StableRoot())
	if err == nil && len(entries) > 0 {
		t.Fatalf("stable root must be empty after a failed clone, found %v", entries)
	}
}

func TestCloneStableClonesAndCanBeReAdded(t *testing.T) {
	isolatedState(t)
	remote, _ := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": validPkg,
	})

	head, err := CloneStable("stump-wtf", remote)
	if err != nil {
		t.Fatalf("clone failed: %v", err)
	}
	if len(head) != 40 {
		t.Fatalf("head must be a full sha, got %q", head)
	}
	if _, err := os.Stat(filepath.Join(PackageDir("stump-wtf", "pr-reviewer"), "package.toml")); err != nil {
		t.Fatalf("clone must hold the package: %v", err)
	}

	// Re-adding the same name replaces the clone (stable remove keeps the
	// directory by design).
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatalf("re-adding a name with an existing clone must work: %v", err)
	}
}

func TestCloneStableRejectsBadNames(t *testing.T) {
	isolatedState(t)
	if _, err := CloneStable("Stump", "https://x/y.git"); err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("bad name must fail, got %v", err)
	}
}

func TestCloneStableRedactsRemoteInError(t *testing.T) {
	isolatedState(t)
	_, err := CloneStable("leaky", "https://user:supersecret@127.0.0.1:1/stable.git")
	if err == nil {
		t.Fatal("clone must fail")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error leaks the remote's userinfo: %v", err)
	}
}

func TestUpdateStableFastForwardAndUpToDate(t *testing.T) {
	isolatedState(t)
	remote, work := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": validPkg,
	})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	res, err := UpdateStable("stump-wtf")
	if err != nil {
		t.Fatalf("update with nothing new must succeed: %v", err)
	}
	if res.FastForwarded {
		t.Fatal("an up-to-date clone must not report a fast-forward")
	}

	writeFiles(t, work, map[string]string{"packages/lint/package.toml": validPkg})
	gitRun(t, work, "add", "-A")
	gitRun(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "add lint")
	gitRun(t, work, "push", "-q", remote, "main")

	res, err = UpdateStable("stump-wtf")
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if !res.FastForwarded {
		t.Fatal("update must fast-forward to the new commit")
	}
	if _, err := os.Stat(filepath.Join(PackageDir("stump-wtf", "lint"), "package.toml")); err != nil {
		t.Fatalf("fast-forward must land the new package: %v", err)
	}
}

// Diverged update: with the remote force-pushed to a diverged history,
// update fails with the diverged-clone sentinel and HEAD, the working tree
// and the index are unchanged.
func TestUpdateStableDiverged(t *testing.T) {
	isolatedState(t)
	remote, work := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": validPkg,
	})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	before := gitRun(t, StableDir("stump-wtf"), "rev-parse", "HEAD")
	statusBefore := gitRun(t, StableDir("stump-wtf"), "status", "--porcelain")

	// Rewrite the remote's history so the clone's HEAD is no longer an
	// ancestor of origin/main.
	gitRun(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--amend", "-m", "rewritten")
	gitRun(t, work, "push", "-q", "-f", remote, "main")

	_, err := UpdateStable("stump-wtf")
	if !errors.Is(err, ErrDivergedClone) {
		t.Fatalf("want ErrDivergedClone, got %v", err)
	}
	after := gitRun(t, StableDir("stump-wtf"), "rev-parse", "HEAD")
	if after != before {
		t.Fatalf("HEAD changed on a refused update: %s -> %s", before, after)
	}
	statusAfter := gitRun(t, StableDir("stump-wtf"), "status", "--porcelain")
	if statusAfter != statusBefore {
		t.Fatalf("working tree or index changed on a refused update: %q -> %q", statusBefore, statusAfter)
	}
}

func TestUpdateStableMissingClone(t *testing.T) {
	isolatedState(t)
	if _, err := UpdateStable("ghost"); err == nil {
		t.Fatal("updating a stable with no clone must fail")
	}
}

func TestListPackages(t *testing.T) {
	isolatedState(t)
	remote, _ := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": validPkg,
		"packages/broken/package.toml":      "not valid toml [[",
		"packages/nomanifest/README.md":     "x",
		"packages/UPPER/README.md":          "x",
	})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	pkgs, err := ListPackages("stump-wtf")
	if err != nil {
		t.Fatalf("ListPackages failed: %v", err)
	}
	if len(pkgs) != 4 {
		t.Fatalf("want 4 discovered entries (valid and invalid), got %d", len(pkgs))
	}
	byName := map[string]Package{}
	for _, p := range pkgs {
		byName[p.Name] = p
	}
	if byName["pr-reviewer"].Manifest == nil || byName["pr-reviewer"].Err != nil {
		t.Errorf("valid package must load, got %+v", byName["pr-reviewer"])
	}
	for _, name := range []string{"broken", "nomanifest", "UPPER"} {
		if p := byName[name]; p.Manifest != nil || p.Err == nil {
			t.Errorf("package %q must carry its load error, got %+v", name, p)
		}
	}
	if !errors.Is(byName["nomanifest"].Err, ErrUnknownPackage) {
		t.Errorf("missing package.toml must wrap ErrUnknownPackage, got %v", byName["nomanifest"].Err)
	}
}

func TestListPackagesEmptyIsNotAnError(t *testing.T) {
	isolatedState(t)
	remote, _ := newStableRemote(t, map[string]string{"packages/.keep": ""})
	if _, err := CloneStable("empty", remote); err != nil {
		t.Fatal(err)
	}
	pkgs, err := ListPackages("empty")
	if err != nil {
		t.Fatalf("an empty packages/ directory is not an error: %v", err)
	}
	if len(pkgs) != 0 {
		t.Fatalf("want zero packages, got %d", len(pkgs))
	}
}

func TestListPackagesMissingDir(t *testing.T) {
	isolatedState(t)
	remote, _ := newStableRemote(t, map[string]string{"README.md": "no packages dir"})
	if _, err := CloneStable("bare", remote); err != nil {
		t.Fatal(err)
	}
	pkgs, err := ListPackages("bare")
	if err != nil {
		t.Fatalf("a missing packages/ directory is not an error: %v", err)
	}
	if len(pkgs) != 0 {
		t.Fatalf("want zero packages, got %d", len(pkgs))
	}
}

func TestLoadPackageAndBundledFiles(t *testing.T) {
	isolatedState(t)
	remote, _ := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml":           validPkg,
		"packages/pr-reviewer/skills/review/SKILL.md": "---\nname: review\n---\nbody",
		"packages/pr-reviewer/prompts/review.md":      "prompt body",
	})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	man, err := LoadPackage("stump-wtf", "pr-reviewer")
	if err != nil {
		t.Fatalf("LoadPackage failed: %v", err)
	}
	if man.Package.Name != "pr-reviewer" || man.Package.Description != "reviews pull requests" {
		t.Fatalf("manifest decoded wrong: %+v", man.Package)
	}

	files, err := BundledFiles("stump-wtf", "pr-reviewer")
	if err != nil {
		t.Fatalf("BundledFiles failed: %v", err)
	}
	want := []string{"package.toml", "prompts/review.md", "skills/review/SKILL.md"}
	if len(files) != len(want) {
		t.Fatalf("want %v, got %v", want, files)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("want %v, got %v", want, files)
		}
	}
}

func TestLoadPackageUnknownPackage(t *testing.T) {
	isolatedState(t)
	remote, _ := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": validPkg,
	})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPackage("stump-wtf", "ghost")
	if !errors.Is(err, ErrUnknownPackage) {
		t.Fatalf("want ErrUnknownPackage, got %v", err)
	}
}

// BundleSkillsDir: the package tier contributes only for a sourced harness
// whose pinned manifest requested skill_paths (SPEC-0026 REQ-10).
func TestBundleSkillsDir(t *testing.T) {
	isoState(t)
	manifestWith := `[package]
name = "pr-reviewer"

[harness]
harness = "claude-code"

[requests]
skill_paths = true
`
	manifestWithout := `[package]
name = "pr-reviewer"

[harness]
harness = "claude-code"
`
	// No source: contributes nothing.
	if dir, ok := BundleSkillsDir(""); ok {
		t.Fatalf("an empty source contributes nothing, got %q", dir)
	}

	// A sourced harness whose manifest requests skill_paths: the pin's
	// bundled skills directory.
	remote, _ := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml":             manifestWith,
		"packages/pr-reviewer/skills/playbook/SKILL.md": "bundled",
	})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	src, err := ResolvePin("stump-wtf", "pr-reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := Materialize(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Place(src, tmp); err != nil {
		t.Fatal(err)
	}

	dir, ok := BundleSkillsDir(src.String())
	if !ok {
		t.Fatal("a requested bundle must contribute its directory")
	}
	if want := filepath.Join(PinDir(src), "skills"); dir != want {
		t.Fatalf("bundle dir = %q, want %q", dir, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "playbook", "SKILL.md")); err != nil {
		t.Fatalf("the bundled skill must be in the pin: %v", err)
	}

	// A manifest that leaves the request unset contributes nothing, even
	// with skills bundled.
	remote2, _ := newStableRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml":             manifestWithout,
		"packages/pr-reviewer/skills/playbook/SKILL.md": "bundled",
	})
	if _, err := CloneStable("stump-wtf", remote2); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePin("stump-wtf", "pr-reviewer", ""); err != nil {
		t.Fatal(err)
	}
	// ResolvePin re-resolves the same tip; simulate the unset case by
	// pointing at the new pin with a manifest that does not request.
	src2, _ := ResolvePin("stump-wtf", "pr-reviewer", "")
	tmp2, _ := Materialize(src2)
	Place(src2, tmp2)
	if _, ok := BundleSkillsDir(src2.String()); ok {
		t.Fatal("an unset request contributes no roots")
	}
}
