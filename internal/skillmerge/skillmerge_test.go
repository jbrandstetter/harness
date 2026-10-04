package skillmerge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, slug, body string) {
	t.Helper()
	dir := filepath.Join(root, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SkillSlug), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The 4-level precedence: a name collision resolves to the highest tier,
// every loser enumerable as shadowed.
func TestResolvePrecedenceAndShadowing(t *testing.T) {
	bundle, global, project, local := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	writeSkill(t, bundle, "playbook", "bundle")
	writeSkill(t, global, "playbook", "global")
	writeSkill(t, project, "playbook", "project")
	writeSkill(t, local, "playbook", "local")
	writeSkill(t, global, "only-global", "g")
	writeSkill(t, project, "only-project", "p")

	set, warns := Resolve([]Root{
		{Dir: bundle, Tier: 0, Source: "package bundle"},
		{Dir: global, Tier: 1, Source: "global skill_paths"},
		{Dir: project, Tier: 2, Source: "project skill_paths"},
		{Dir: local, Tier: 3, Source: "project local"},
	})
	if len(warns) != 0 {
		t.Fatalf("no warnings expected: %v", warns)
	}
	if len(set) != 3 {
		t.Fatalf("want 3 skill names, got %d: %v", len(set), set)
	}
	pb := set["playbook"]
	if pb.Winner != filepath.Join(local, "playbook") {
		t.Fatalf("the highest tier must win: %+v", pb)
	}
	if len(pb.Shadowed) != 3 {
		t.Fatalf("three shadowed copies must be enumerable, got %+v", pb.Shadowed)
	}
	// Shadowed order is lowest tier first, each carrying its source label.
	if pb.Shadowed[0].Source != "package bundle" || pb.Shadowed[2].Source != "project skill_paths" {
		t.Fatalf("shadowed copies must carry their sources in order: %+v", pb.Shadowed)
	}
	if _, ok := set["only-global"]; !ok {
		t.Fatal("an unshadowed skill must resolve")
	}
}

// An unreadable root warns and never blocks; a missing root is silent.
func TestResolveUnreadableRoot(t *testing.T) {
	ok := t.TempDir()
	writeSkill(t, ok, "a", "a")

	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	missing := filepath.Join(t.TempDir(), "missing")

	set, warns := Resolve([]Root{
		{Dir: locked, Tier: 0, Source: "global skill_paths"},
		{Dir: missing, Tier: 1, Source: "global skill_paths"},
		{Dir: ok, Tier: 2, Source: "project local"},
	})
	if len(set) != 1 {
		t.Fatalf("the readable root must still merge, got %v", set)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Error(), locked) {
		t.Fatalf("the unreadable root must warn naming the path, got %v", warns)
	}
}

// Projection copies contents — the projected file is a real file, so a
// harness editing it cannot mutate the source root.
func TestProjectCopiesNotLinks(t *testing.T) {
	src := t.TempDir()
	writeSkill(t, src, "a", "body")
	target := filepath.Join(t.TempDir(), "target")

	set, _ := Resolve([]Root{{Dir: src, Tier: 1, Source: "global skill_paths"}})
	if err := Project(target, set); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(target, "a", SkillSlug)
	raw, err := os.ReadFile(proj)
	if err != nil {
		t.Fatalf("projected skill missing: %v", err)
	}
	if string(raw) != "body" {
		t.Fatalf("projected content wrong: %q", raw)
	}
	fi, err := os.Lstat(proj)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("projection must copy, never symlink")
	}

	// Editing the projection leaves the source untouched.
	if err := os.WriteFile(proj, []byte("mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(src, "a", SkillSlug))
	if string(after) != "body" {
		t.Fatalf("projection must isolate the source: got %q", after)
	}
}

// A symlink inside a source skill is refused, not followed.
func TestProjectRefusesSymlinks(t *testing.T) {
	src := t.TempDir()
	writeSkill(t, src, "a", "body")
	if err := os.Symlink(filepath.Join(src, "a", "extra.txt"), filepath.Join(src, "a", "link.txt")); err != nil {
		t.Fatal(err)
	}
	set, _ := Resolve([]Root{{Dir: src, Tier: 1, Source: "global"}})
	err := Project(filepath.Join(t.TempDir(), "target"), set)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlink must be refused, got %v", err)
	}
}

// A target that cannot be written fails with the sentinel, naming it.
func TestProjectTargetNotWritable(t *testing.T) {
	set, _ := Resolve([]Root{{Dir: t.TempDir(), Tier: 1, Source: "global"}})
	writeSkill(t, t.TempDir(), "a", "x")
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("a file, not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Project(blocked, set)
	if !errors.Is(err, ErrTargetNotWritable) {
		t.Fatalf("want ErrTargetNotWritable, got %v", err)
	}
	if !strings.Contains(err.Error(), blocked) {
		t.Fatalf("the error must name the target: %v", err)
	}
}

// Content already in the target that the merge did not produce is the
// tool's own concern: the merge never deletes what it does not own.
func TestProjectNeverDeletes(t *testing.T) {
	target := t.TempDir()
	own := filepath.Join(target, "tool-own-skill")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, SkillSlug), []byte("tool's own"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := t.TempDir()
	writeSkill(t, src, "a", "body")
	set, _ := Resolve([]Root{{Dir: src, Tier: 1, Source: "global"}})
	if err := Project(target, set); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(own, SkillSlug)); err != nil {
		t.Fatalf("the tool's own skill must survive projection: %v", err)
	}
}
