// See doc.go for the package's purpose and governing artifacts.
package skillstore

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Status is the lifecycle state of a learned skill (SPEC-0007 REQ
// "Two-Channel Gate").
type Status string

const (
	// StatusProposed is a freshly authored skill. It appears in no
	// harness's projected skill set and is returned by no search.
	StatusProposed Status = "proposed"
	// StatusPromoted is a skill that passed the promotion gate. It is
	// indexed for search, and still projected into no harness.
	StatusPromoted Status = "promoted"
	// StatusSuperseded marks a skill replaced by a successor. It leaves
	// the index; its frontmatter records the successor's identifier.
	StatusSuperseded Status = "superseded"
	// StatusRetired marks a skill withdrawn from the index. The markdown
	// file remains.
	StatusRetired Status = "retired"
)

// Level is the granularity of a learned skill.
type Level string

const (
	LevelAtomic    Level = "atomic"
	LevelComposite Level = "composite"
	LevelPattern   Level = "pattern"
)

// The closed vocabularies (SPEC-0007 REQ "Skill Artifact"). An
// out-of-vocabulary value MUST NOT be coerced.
var TaskFamilies = []string{
	"aggregation", "configuration", "dispatch", "initialization", "io",
	"lookup", "orchestration", "parsing", "recovery", "security",
	"state_transition", "transformation", "troubleshooting", "validation",
	"verification", "workflow",
}

var Actions = []string{
	"add", "remove", "rename", "configure", "pin", "migrate", "validate",
	"retry", "handle_error", "order", "escape", "authenticate", "release",
	"test", "document",
}

var Targets = []string{
	"dependency", "build", "ci", "config", "schema", "api", "cli", "test",
	"auth", "network", "filesystem", "concurrency", "logging", "docs",
	"release",
}

// MaxDescription is the frontmatter description cap.
const MaxDescription = 300

// Provenance records where a learned skill came from (SPEC-0007 REQ "Skill
// Artifact"): the contributing trajectories, the distinct project count, and
// the first- and last-seen timestamps.
type Provenance struct {
	// Projects is the count of distinct projects that contributed
	// occurrences to the cluster.
	Projects int `yaml:"projects"`
	// FirstSeen is the earliest contributing occurrence, RFC 3339.
	FirstSeen string `yaml:"first_seen,omitempty"`
	// LastSeen is the latest contributing occurrence, RFC 3339.
	LastSeen string `yaml:"last_seen,omitempty"`
	// Trajectories lists the contributing sessions as
	// "harness/session-id" pairs.
	Trajectories []string `yaml:"trajectories,omitempty"`
}

// Frontmatter is the learned skill's YAML frontmatter.
type Frontmatter struct {
	Name         string     `yaml:"name"`
	Description  string     `yaml:"description"`
	Level        Level      `yaml:"level"`
	Status       Status     `yaml:"status"`
	TaskFamily   string     `yaml:"task_family"`
	Action       string     `yaml:"action"`
	Target       string     `yaml:"target"`
	Tags         []string   `yaml:"tags,omitempty"`
	PurposeKey   string     `yaml:"purpose_key"`
	Symptoms     []string   `yaml:"symptoms"`
	AppliesTo    []string   `yaml:"applies_to,omitempty"`
	SupersededBy string     `yaml:"superseded_by,omitempty"`
	Provenance   Provenance `yaml:"provenance"`
}

// requiredSections are the body sections SPEC-0007 REQ "Skill Artifact"
// requires of every learned skill.
var requiredSections = []string{
	"When to use", "Steps", "Invariants", "Failure modes", "Do not", "Evidence",
}

// Skill is one learned skill: its frontmatter and markdown body.
type Skill struct {
	Frontmatter Frontmatter
	Body        string
}

// Parse parses SKILL.md content into a Skill, validating the frontmatter
// against the closed vocabularies and the body against the required
// sections. Errors wrap ErrMalformedFrontmatter and name the failing rule.
func Parse(content string) (Skill, error) {
	s, err := parse(content)
	if err != nil {
		return Skill{}, fmt.Errorf("%w: %w", ErrMalformedFrontmatter, err)
	}
	return s, nil
}

func parse(content string) (Skill, error) {
	rest := strings.TrimPrefix(content, "---\n")
	if rest == content {
		return Skill{}, errors.New("missing frontmatter delimiter")
	}
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		idx = strings.Index(rest, "\n---\r\n")
	}
	if idx < 0 {
		return Skill{}, errors.New("unterminated frontmatter block")
	}
	fm, body := rest[:idx], rest[idx+len("\n---\n"):]

	var f Frontmatter
	if err := yaml.Unmarshal([]byte(fm), &f); err != nil {
		return Skill{}, fmt.Errorf("frontmatter: %w", err)
	}
	s := Skill{Frontmatter: f, Body: body}
	if err := s.Validate(); err != nil {
		return Skill{}, err
	}
	return s, nil
}

// Validate checks the frontmatter and body against SPEC-0007 REQ "Skill
// Artifact". Every failure names the file-independent rule it broke.
func (s Skill) Validate() error {
	f := s.Frontmatter
	if f.Name == "" {
		return errors.New("name is required")
	}
	if f.Description == "" {
		return errors.New("description is required")
	}
	if len(f.Description) > MaxDescription {
		return fmt.Errorf("description exceeds %d characters", MaxDescription)
	}
	switch f.Level {
	case LevelAtomic, LevelComposite, LevelPattern:
	default:
		return fmt.Errorf("level %q is outside the vocabulary", f.Level)
	}
	switch f.Status {
	case StatusProposed, StatusPromoted, StatusSuperseded, StatusRetired:
	default:
		return fmt.Errorf("status %q is outside the vocabulary", f.Status)
	}
	if f.Status == StatusSuperseded && f.SupersededBy == "" {
		return errors.New("superseded requires superseded_by")
	}
	if f.Status != StatusSuperseded && f.SupersededBy != "" {
		return errors.New("superseded_by requires status superseded")
	}
	if !contains(TaskFamilies, f.TaskFamily) {
		return fmt.Errorf("task_family %q is outside the vocabulary", f.TaskFamily)
	}
	if !contains(Actions, f.Action) {
		return fmt.Errorf("action %q is outside the vocabulary", f.Action)
	}
	if !contains(Targets, f.Target) {
		return fmt.Errorf("target %q is outside the vocabulary", f.Target)
	}
	if f.PurposeKey == "" {
		return errors.New("purpose_key is required")
	}
	if len(f.Symptoms) == 0 {
		return errors.New("symptoms is required and must be non-empty")
	}
	if f.Provenance.Projects < 1 {
		return errors.New("provenance.projects must be at least 1")
	}
	for _, section := range requiredSections {
		if !hasSection(s.Body, section) {
			return fmt.Errorf("body is missing the %q section", section)
		}
	}
	return nil
}

// hasSection reports whether the body carries a "## <section>" heading.
func hasSection(body, section string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimRight(line, " \t\r") == "## "+section {
			return true
		}
	}
	return false
}

// Slug derives the skill's directory name from its purpose key
// (SPEC-0007 REQ "Evidence Key, Purpose Key And Scope": "The slug SHALL be
// derived from the purpose key"). It is deterministic: the purpose key's
// non-empty parts, lowercased, joined with dashes.
func Slug(purposeKey string) string {
	parts := strings.Split(purposeKey, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "-")
}

// Path returns the skill's path inside the store: <slug>/SKILL.md.
func (s Skill) Path() string {
	return Slug(s.Frontmatter.PurposeKey) + "/SKILL.md"
}

// Indexable reports whether the skill may enter the retrieval index
// (SPEC-0007 REQ "Two-Channel Gate": only promoted skills are indexed).
func (s Skill) Indexable() bool {
	return s.Frontmatter.Status == StatusPromoted
}

// Projectable reports whether the skill may be projected into a harness's
// native skill directory. The learned tier is never projected, at any
// status (SPEC-0007 REQ "Two-Channel Gate"): the SPEC-0006 projection path
// must exclude it unconditionally.
func (s Skill) Projectable() bool {
	return false
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}
