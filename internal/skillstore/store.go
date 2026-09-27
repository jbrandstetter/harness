// See skill.go for the package's purpose and governing artifacts.
package skillstore

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/stump-wtf/harness/internal/supervisor"
)

// DefaultDir is the learned store's location under the state directory:
// $XDG_STATE_HOME/harness/skills/learned. ADR-0030's serving clones live at
// skills/<name>, so the learned tier never collides with a skill repo name
// unless an operator names one `learned`.
func DefaultDir() string {
	return filepath.Join(supervisor.StateHome(), "skills", "learned")
}

// Sentinels (SPEC-0007 REQ "Error Handling Standards").
var (
	// ErrSkillNotFound is returned when no skill exists at the requested
	// path or identifier.
	ErrSkillNotFound = errors.New("skillstore: skill not found")
	// ErrMalformedFrontmatter is returned when a skill file cannot be
	// parsed or fails validation.
	ErrMalformedFrontmatter = errors.New("skillstore: malformed skill")
	// ErrGit is returned when a git operation on the store fails.
	ErrGit = errors.New("skillstore: git operation failed")
)

// Store is the learned skill store: a git-tracked markdown directory under
// the Harness state directory. One directory per skill, holding SKILL.md.
// The store is written by authoring commands (the distiller's CLI paths),
// never by the daemon: the daemon MUST NOT write to any repository working
// tree, and this package takes no daemon dependency.
//
// The store is its own git repository, initialized on first use. Nothing is
// ever pushed; the git history is the store's audit trail.
type Store struct {
	root string
}

// New returns a Store rooted at dir, without touching the filesystem. Most
// callers want Open, which also initializes the directory.
func New(dir string) *Store {
	return &Store{root: dir}
}

// Root returns the store's directory path.
func (s *Store) Root() string { return s.root }

// Open initializes the store directory as a git repository if it does not
// already exist. It is idempotent.
func (s *Store) Open() error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("skillstore: create %s: %w", s.root, err)
	}
	if _, err := os.Stat(filepath.Join(s.root, ".git")); err == nil {
		return nil
	}
	if err := s.git("init", "-q"); err != nil {
		return err
	}
	// Anchor the repository so the first skill write has a parent commit.
	if err := s.git("commit", "--allow-empty", "-q", "-m", "skillstore: init"); err != nil {
		return err
	}
	return nil
}

// git runs a git command inside the store, returning ErrGit-wrapped errors.
func (s *Store) git(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = s.root
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=harness", "GIT_AUTHOR_EMAIL=harness@localhost",
		"GIT_COMMITTER_NAME=harness", "GIT_COMMITTER_EMAIL=harness@localhost")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: git %s: %s", ErrGit, args[0], strings.TrimSpace(string(out)))
	}
	return nil
}

// Write serializes the skill to <slug>/SKILL.md and commits the change.
// Writing never touches any path outside the store directory.
func (s *Store) Write(skill Skill) error {
	if err := skill.Validate(); err != nil {
		return fmt.Errorf("skillstore: %s: %w", skill.Path(), err)
	}
	rel := skill.Path()
	abs := filepath.Join(s.root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("skillstore: create %s: %w", filepath.Dir(rel), err)
	}
	if err := os.WriteFile(abs, []byte(skill.Render()), 0o644); err != nil {
		return fmt.Errorf("skillstore: write %s: %w", rel, err)
	}
	if err := s.git("add", "--", rel); err != nil {
		return err
	}
	msg := fmt.Sprintf("skill: %s (%s)", skill.Frontmatter.Name, skill.Frontmatter.Status)
	return s.git("commit", "-q", "-m", msg, "--", rel)
}

// Read returns the skill at <slug>/SKILL.md, or ErrSkillNotFound.
func (s *Store) Read(slug string) (Skill, error) {
	content, err := os.ReadFile(filepath.Join(s.root, slug, "SKILL.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return Skill{}, fmt.Errorf("%w: %s", ErrSkillNotFound, slug)
		}
		return Skill{}, fmt.Errorf("skillstore: read %s: %w", slug, err)
	}
	skill, err := Parse(string(content))
	if err != nil {
		return Skill{}, fmt.Errorf("skillstore: %s/SKILL.md: %w", slug, err)
	}
	return skill, nil
}

// List returns every skill in the store, sorted by slug. A file that fails
// to parse is skipped and reported as a warning through the returned error
// list rather than aborting the listing (SPEC-0007 REQ "Error Handling
// Standards": one malformed skill must not break the index).
func (s *Store) List() ([]Skill, []error) {
	slugs, err := s.Slugs()
	if err != nil {
		return nil, []error{err}
	}
	var out []Skill
	var bad []error
	for _, slug := range slugs {
		skill, err := s.Read(slug)
		if err != nil {
			bad = append(bad, err)
			continue
		}
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool {
		return Slug(out[i].Frontmatter.PurposeKey) < Slug(out[j].Frontmatter.PurposeKey)
	})
	return out, bad
}

// Slugs lists the skill directories (those holding a SKILL.md), sorted.
func (s *Store) Slugs() ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("skillstore: list %s: %w", s.root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.root, e.Name(), "SKILL.md")); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// Render serializes the skill to its SKILL.md form.
func (s Skill) Render() string {
	fm, err := yaml.Marshal(s.Frontmatter)
	if err != nil {
		// Frontmatter marshaling of this struct cannot fail; panic-free
		// fallback keeps Render total.
		return "---\n---\n" + s.Body
	}
	return "---\n" + string(fm) + "---\n" + s.Body
}

// StatusAt is a convenience for tests and callers reporting age: the parsed
// first-seen timestamp, or the zero time.
func (s Skill) FirstSeen() (time.Time, bool) {
	return parseRFC3339(s.Frontmatter.Provenance.FirstSeen)
}

// LastSeen is the parsed last-seen timestamp, or the zero time.
func (s Skill) LastSeen() (time.Time, bool) {
	return parseRFC3339(s.Frontmatter.Provenance.LastSeen)
}

func parseRFC3339(v string) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
