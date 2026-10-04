package agentpkg

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stump-wtf/harness/internal/gitcmd"
)

func isoState(t *testing.T) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	// The store is deliberately read-only, so t.TempDir's cleanup cannot
	// remove it unaided.
	t.Cleanup(func() {
		_ = chmodTreeWritable(StateHome())
		_ = os.RemoveAll(StateHome())
	})
}

func gitRun2(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitcmd.Run(dir, args...)
	if err != nil {
		t.Fatalf("git %v (in %s): %v: %s", args, dir, err, out)
	}
	return out
}

// newPkgRemote builds a bare repository whose packages/<name>/ carries the
// files, and returns the bare remote path plus the work tree.
func newPkgRemote(t *testing.T, pkg string, files map[string]string) (remote, work string) {
	t.Helper()
	work = t.TempDir()
	gitRun2(t, work, "init", "-q", "-b", "main")
	for rel, body := range files {
		path := filepath.Join(work, "packages", pkg, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun2(t, work, "add", "-A")
	gitRun2(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "fixture")
	remote = filepath.Join(t.TempDir(), "remote.git")
	gitRun2(t, work, "clone", "-q", "--bare", work, remote)
	return remote, work
}

const goodManifest = `[package]
name = "pr-reviewer"
description = "reviews pull requests"

[harness]
harness = "claude-code"
`

// A branch resolves to a pinned SHA: the default (no version) is the
// default branch tip, and the result is a full 40-hex SHA, never a branch
// name (REQ-6).
func TestResolvePinDefaultBranchTip(t *testing.T) {
	isoState(t)
	remote, _ := newPkgRemote(t, "pr-reviewer", map[string]string{"package.toml": goodManifest})
	if _, err := CloneStable("stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	src, err := ResolvePin("stump-wtf", "pr-reviewer", "")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if !shaRe.MatchString(src.SHA) {
		t.Fatalf("source must carry a full 40-hex sha, got %q", src.SHA)
	}
	want := gitRun2(t, StableDir("stump-wtf"), "rev-parse", "origin/main")
	want = trimSpace(want)
	if src.SHA != want {
		t.Fatalf("default resolves to the default branch tip: got %s want %s", src.SHA, want)
	}

	// An explicit version resolves through the clone too.
	if _, err := ResolvePin("stump-wtf", "pr-reviewer", "main"); err != nil {
		t.Fatalf("resolve by branch name: %v", err)
	}
}

// A version beginning with "-" is refused before it can reach git: fed to
// rev-parse it would be parsed as an option, not a ref.
func TestResolvePinRefusesOptionLikeVersion(t *testing.T) {
	if _, err := ResolvePin("stump-wtf", "pr-reviewer", "--flags"); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("an option-like version must be refused as an invalid source, got %v", err)
	}
}

// A full SHA already in the store is used directly, without consulting the
// clone at all (REQ-6).
func TestResolvePinFullSHAInStoreSkipsClone(t *testing.T) {
	isoState(t)
	remote, _ := newPkgRemote(t, "pr-reviewer", map[string]string{"package.toml": goodManifest})
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
	// The clone is gone; the store answers alone.
	if err := os.RemoveAll(StableDir("stump-wtf")); err != nil {
		t.Fatal(err)
	}
	again, err := ResolvePin("stump-wtf", "pr-reviewer", src.SHA)
	if err != nil {
		t.Fatalf("a stored sha must resolve without the clone: %v", err)
	}
	if again != src {
		t.Fatalf("got %v want %v", again, src)
	}
}

func TestResolvePinNoCloneNoStore(t *testing.T) {
	isoState(t)
	if _, err := ResolvePin("ghost", "pkg", ""); err == nil {
		t.Fatal("resolving with no clone and no store must fail")
	}
}

// Materialize reads git objects, never the working tree; the extracted
// tree is read-only; Place renames it in; an existing pin is reused
// untouched.
func TestMaterializeAndPlace(t *testing.T) {
	isoState(t)
	remote, _ := newPkgRemote(t, "pr-reviewer", map[string]string{
		"package.toml":           goodManifest,
		"skills/review/SKILL.md": "body",
		"prompts/review.md":      "prompt",
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
	for _, rel := range []string{"package.toml", "skills/review/SKILL.md", "prompts/review.md"} {
		info, err := os.Stat(filepath.Join(tmp, rel))
		if err != nil {
			t.Fatalf("materialized tree missing %s: %v", rel, err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Fatalf("materialized file %s must be read-only, got %v", rel, info.Mode().Perm())
		}
	}

	reused, err := Place(src, tmp)
	if err != nil || reused {
		t.Fatalf("first place must adopt the temp dir (reused=%v err=%v)", reused, err)
	}
	before, err := PinStat(src)
	if err != nil {
		t.Fatal(err)
	}

	// A second materialization of the same pin reuses the store entry: the
	// pin directory's identity and mtime are unchanged.
	tmp2, err := Materialize(src)
	if err != nil {
		t.Fatal(err)
	}
	reused, err = Place(src, tmp2)
	if err != nil || !reused {
		t.Fatalf("second place must reuse the pin (reused=%v err=%v)", reused, err)
	}
	after, err := PinStat(src)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("the pin directory must not be replaced on a repeated install")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("the pin directory must not be rewritten on a repeated install")
	}
}

// Prune removes only unreferenced pins, takes the record file with them,
// and leaves no empty parents behind.
func TestPrune(t *testing.T) {
	isoState(t)
	remote, _ := newPkgRemote(t, "pr-reviewer", map[string]string{"package.toml": goodManifest})
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
	// A record beside the pin.
	if err := WriteInstallRecord(src, InstallRecord{Source: src.String(), ScannerVersion: "1"}); err != nil {
		t.Fatal(err)
	}

	// Referenced: nothing pruned, record stays.
	removed, err := Prune(map[Source]bool{src: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("referenced pin must stay, removed %v", removed)
	}
	if _, err := os.Stat(RecordPath(src)); err != nil {
		t.Fatal("record beside a kept pin must stay")
	}

	// Unreferenced: pin and record go, parents cleaned up.
	removed, err = Prune(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != src.String() {
		t.Fatalf("want the pin removed, got %v", removed)
	}
	if _, err := os.Stat(PinDir(src)); !os.IsNotExist(err) {
		t.Fatal("pin directory must be gone")
	}
	if _, err := os.Stat(RecordPath(src)); !os.IsNotExist(err) {
		t.Fatal("record file must go with the pin")
	}
	if _, err := os.Stat(filepath.Join(InstalledRoot(), "stump-wtf")); !os.IsNotExist(err) {
		t.Fatal("empty stable directory must be cleaned up")
	}
}

// PinOf maps a path to the pin directory holding it: the pin directory
// itself or anything beneath it, and nothing else — not a relative path, a
// stable or package parent, a temp sibling, or a path that only climbs
// back in through "..".
func TestPinOf(t *testing.T) {
	isoState(t)
	sha := "0123456789abcdef0123456789abcdef01234567"
	want := Source{Stable: "stump-wtf", Package: "pr-reviewer", SHA: sha}
	pin := PinDir(want)
	for _, p := range []string{pin, filepath.Join(pin, "system.md"), filepath.Join(pin, "prompts", "review.md"), pin + "/./mcp.json"} {
		got, ok := PinOf(p)
		if !ok || got != want {
			t.Errorf("PinOf(%q) = %v, %v; want %v", p, got, ok, want)
		}
	}
	root := InstalledRoot()
	for _, p := range []string{
		"",
		filepath.Join("stump-wtf", "pr-reviewer", sha, "system.md"),
		filepath.Join(root, "stump-wtf", "pr-reviewer"),
		filepath.Join(root, "stump-wtf", "pr-reviewer", ".tmp-123", "system.md"),
		filepath.Join(root, "stump-wtf", "pr-reviewer", "main", "system.md"),
		filepath.Join(root, "..", "installed-not", "stump-wtf", "pr-reviewer", sha),
		filepath.Join(t.TempDir(), "stump-wtf", "pr-reviewer", sha, "system.md"),
	} {
		if got, ok := PinOf(p); ok {
			t.Errorf("PinOf(%q) = %v; want no pin", p, got)
		}
	}
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
