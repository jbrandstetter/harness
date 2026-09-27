// Tests for the learned skill store: frontmatter parse/serialize with
// fixtures, the two-channel gate, and the git-tracked store layout
// (SPEC-0007 REQ "Skill Artifact", REQ "Two-Channel Gate").
//
// Governing: SPEC-0007; ADR-0012.
//
// @joestump-agent 09/27/2026 - Added for harness#77.
package skillstore

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func validSkill() Skill {
	return Skill{
		Frontmatter: Frontmatter{
			Name:        "pin-go-version-in-ci",
			Description: "Pin the Go toolchain in CI before bumping go.mod.",
			Level:       LevelAtomic,
			Status:      StatusPromoted,
			TaskFamily:  "configuration",
			Action:      "pin",
			Target:      "ci",
			PurposeKey:  "configuration/pin/ci/go-mod",
			Symptoms:    []string{"go: cannot find main module"},
			AppliesTo:   []string{".gitea/workflows/*"},
			Provenance: Provenance{
				Projects:     3,
				FirstSeen:    "2026-09-01T10:00:00Z",
				LastSeen:     "2026-09-20T18:30:00Z",
				Trajectories: []string{"reduit/agent/a1b2c3"},
			},
		},
		Body: "## When to use\n\nx\n\n## Steps\n\nx\n\n## Invariants\n\nx\n\n" +
			"## Failure modes\n\nx\n\n## Do not\n\nx\n\n## Evidence\n\nx\n",
	}
}

func TestParseGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/promoted.golden.md")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	skill, err := Parse(string(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := skill.Frontmatter.Name, "pin-go-version-in-ci"; got != want {
		t.Errorf("name = %q, want %q", got, want)
	}
	if !skill.Indexable() {
		t.Errorf("promoted fixture should be indexable")
	}
	if got, want := skill.Path(), "configuration-pin-ci-go-mod/SKILL.md"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

func TestParseRenderRoundTrip(t *testing.T) {
	skill := validSkill()
	parsed, err := Parse(skill.Render())
	if err != nil {
		t.Fatalf("parse render: %v", err)
	}
	if parsed.Frontmatter.Name != skill.Frontmatter.Name ||
		parsed.Frontmatter.Status != skill.Frontmatter.Status ||
		parsed.Frontmatter.PurposeKey != skill.Frontmatter.PurposeKey ||
		len(parsed.Frontmatter.Symptoms) != len(skill.Frontmatter.Symptoms) {
		t.Errorf("frontmatter round-trip mismatch:\n got %+v\nwant %+v", parsed.Frontmatter, skill.Frontmatter)
	}
	if parsed.Body != skill.Body {
		t.Errorf("body round-trip mismatch")
	}
}

func TestParseMalformed(t *testing.T) {
	cases := map[string]string{
		"no frontmatter":   "just a body",
		"unterminated":     "---\nname: x\n",
		"bad yaml":         "---\nname: [unclosed\n---\n",
		"missing name":     "---\ndescription: x\nlevel: atomic\nstatus: promoted\ntask_family: io\naction: add\ntarget: ci\npurpose_key: io/add/ci\nsymptoms: [e]\nprovenance: {projects: 1}\n---\n",
		"long description": mustLong(),
		"bad level":        mustField("level", "mega"),
		"bad status":       mustField("status", "draft"),
		"bad task_family":  mustField("task_family", "misc"),
		"bad action":       mustField("action", "refactor"),
		"bad target":       mustField("target", "misc"),
		"no purpose_key":   mustField("purpose_key", ""),
		"no symptoms":      mustField("symptoms", "[]"),
		"zero projects":    mustField("projects", "0"),
		"missing section":  mustSection(),
		"superseded no by": mustField("status", "superseded"),
		"by without super": mustField("superseded_by", "other"),
	}
	for name, raw := range cases {
		_, err := Parse(raw)
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
			continue
		}
		if !errors.Is(err, ErrMalformedFrontmatter) {
			t.Errorf("%s: error %v does not wrap ErrMalformedFrontmatter", name, err)
		}
	}
}

func mustLong() string {
	s := validSkill()
	s.Frontmatter.Description = strings.Repeat("x", MaxDescription+1)
	return s.Render()
}

func mustField(field, value string) string {
	s := validSkill()
	switch field {
	case "level":
		s.Frontmatter.Level = Level(value)
	case "status":
		s.Frontmatter.Status = Status(value)
	case "task_family":
		s.Frontmatter.TaskFamily = value
	case "action":
		s.Frontmatter.Action = value
	case "target":
		s.Frontmatter.Target = value
	case "purpose_key":
		s.Frontmatter.PurposeKey = value
	case "symptoms":
		s.Frontmatter.Symptoms = nil
	case "projects":
		s.Frontmatter.Provenance.Projects = 0
	case "superseded_by":
		s.Frontmatter.SupersededBy = value
	}
	return s.Render()
}

func mustSection() string {
	s := validSkill()
	s.Body = strings.Replace(s.Body, "## Do not\n", "", 1)
	return s.Render()
}

func TestTwoChannelGate(t *testing.T) {
	for _, tc := range []struct {
		status    Status
		indexable bool
	}{
		{StatusProposed, false},
		{StatusPromoted, true},
		{StatusSuperseded, false},
		{StatusRetired, false},
	} {
		s := validSkill()
		s.Frontmatter.Status = tc.status
		if got := s.Indexable(); got != tc.indexable {
			t.Errorf("status %s: indexable = %v", tc.status, got)
		}
		if s.Projectable() {
			t.Errorf("status %s: learned tier must never be projectable", tc.status)
		}
	}
}

func TestSlugFromPurposeKey(t *testing.T) {
	if got, want := Slug("configuration/pin/ci/go-mod"), "configuration-pin-ci-go-mod"; got != want {
		t.Errorf("Slug = %q, want %q", got, want)
	}
	if got := Slug("/a//b/"); got != "a-b" {
		t.Errorf("Slug empty parts = %q", got)
	}
}

func TestStoreWriteReadListCommit(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(dir + "/.git"); err != nil {
		t.Fatalf("store is not a git repository: %v", err)
	}

	skill := validSkill()
	if err := store.Write(skill); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := store.Read("configuration-pin-ci-go-mod")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Frontmatter.Name != skill.Frontmatter.Name {
		t.Errorf("read back wrong skill")
	}

	proposed := skill
	proposed.Frontmatter.Status = StatusProposed
	proposed.Frontmatter.PurposeKey = "validation/add/config/go-mod"
	if err := store.Write(proposed); err != nil {
		t.Fatalf("write proposed: %v", err)
	}

	all, bad := store.List()
	if len(bad) != 0 {
		t.Errorf("unexpected parse warnings: %v", bad)
	}
	if len(all) != 2 {
		t.Fatalf("List returned %d skills, want 2", len(all))
	}
	if all[0].Frontmatter.PurposeKey != "configuration/pin/ci/go-mod" {
		t.Errorf("List is not sorted by slug: first = %s", all[0].Frontmatter.PurposeKey)
	}

	if _, err := store.Read("nope"); !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("Read missing = %v, want ErrSkillNotFound", err)
	}

	// The store's history records the writes (git-tracked, never pushed).
	out, err := exec.Command("git", "-C", dir, "log", "--oneline").CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	for _, want := range []string{"skillstore: init", "skill: pin-go-version-in-ci (promoted)"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("history missing %q:\n%s", want, out)
		}
	}
}

func TestListSkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Write(validSkill()); err != nil {
		t.Fatalf("write: %v", err)
	}
	badDir := dir + "/broken-thing"
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badDir+"/SKILL.md", []byte("---\nstatus: nope\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	all, bad := store.List()
	if len(all) != 1 {
		t.Errorf("good skill lost to malformed peer: got %d", len(all))
	}
	if len(bad) != 1 {
		t.Errorf("malformed skill not reported: %v", bad)
	}
}

func TestStoreWriteRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	if err := store.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	skill := validSkill()
	skill.Frontmatter.Action = "refactor"
	if err := store.Write(skill); err == nil {
		t.Errorf("Write accepted an out-of-vocabulary action")
	}
}
