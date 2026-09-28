// Skill-repo skill files: the <path>/<slug>/SKILL.md artifact format the
// daemon indexes (SPEC-0007 REQ "Skill Artifact"). These files live in a
// skill repo, so their lifecycle status vocabulary is active/retired, unlike
// the learned store's proposed/promoted/superseded/retired.
//
// Governing: SPEC-0007 REQ "Skill Artifact", REQ "Default-Branch Gate", REQ
// "Error Handling Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/stump-wtf/harness/internal/skillstore"
)

// Skill statuses valid in a skill repo (SPEC-0007 REQ "Skill Artifact").
const (
	StatusActive  = "active"
	StatusRetired = "retired"
)

// Entry is one skill file's indexed content, already reduced to the fields the
// retrieval index covers.
type Entry struct {
	Repo        string
	Slug        string
	RelPath     string
	Name        string
	Description string
	Symptoms    []string
	Tags        []string
	AppliesTo   []string
	// MergedAt is the file's last commit time on the default branch, unix
	// seconds. It grounds retirement eligibility.
	MergedAt int64
}

// repoFrontmatter is the subset of the skill artifact's frontmatter the
// retrieval index reads. The field names follow SPEC-0007 REQ "Skill
// Artifact".
type repoFrontmatter struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Status      string   `yaml:"status"`
	Tags        []string `yaml:"tags"`
	Symptoms    []string `yaml:"symptoms"`
	AppliesTo   []string `yaml:"applies_to"`
}

// ParseSkillFile parses one SKILL.md from a serving clone. A file whose
// frontmatter cannot be parsed, whose status is outside the active/retired
// vocabulary, or that fails the artifact rules returns an error wrapping
// skillstore.ErrMalformedFrontmatter.
func ParseSkillFile(content string) (repoFrontmatter, error) {
	rest := strings.TrimPrefix(content, "---\n")
	if rest == content {
		return repoFrontmatter{}, errors.New("missing frontmatter delimiter")
	}
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		idx = strings.Index(rest, "\n---\r\n")
	}
	if idx < 0 {
		return repoFrontmatter{}, errors.New("unterminated frontmatter block")
	}
	var fm repoFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
		return repoFrontmatter{}, fmt.Errorf("frontmatter: %w", err)
	}
	if strings.TrimSpace(fm.Name) == "" {
		return repoFrontmatter{}, errors.New("name is required")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return repoFrontmatter{}, errors.New("description is required")
	}
	if len(fm.Description) > skillstore.MaxDescription {
		return repoFrontmatter{}, fmt.Errorf("description exceeds %d characters", skillstore.MaxDescription)
	}
	switch fm.Status {
	case StatusActive, StatusRetired:
	default:
		return repoFrontmatter{}, fmt.Errorf("status %q is outside the vocabulary", fm.Status)
	}
	if len(fm.Symptoms) == 0 {
		return repoFrontmatter{}, errors.New("symptoms is required and must be non-empty")
	}
	return fm, nil
}

// collectEntries walks a clone's configured path and returns an entry for
// every active skill file. A malformed file is returned in the warnings list
// naming the file and the cause rather than aborting the walk (SPEC-0007 REQ
// "Error Handling Standards").
func collectEntries(repo, cloneDir, path string) ([]Entry, []string, error) {
	root := filepath.Join(cloneDir, path)
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("skillserve: repo %s: skill path %s: %w", repo, path, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("skillserve: repo %s: skill path %s is not a directory", repo, path)
	}

	var entries []Entry
	var warnings []string
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", p, err))
			return nil
		}
		if d.IsDir() || d.Name() != "SKILL.md" {
			return nil
		}
		rel, relErr := filepath.Rel(cloneDir, p)
		if relErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", p, relErr))
			return nil
		}
		content, readErr := os.ReadFile(p)
		if readErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", rel, readErr))
			return nil
		}
		fm, parseErr := ParseSkillFile(string(content))
		if parseErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", rel, parseErr))
			return nil
		}
		if fm.Status != StatusActive {
			return nil
		}
		mergedAt, gitErr := MergedAt(cloneDir, rel)
		if gitErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", rel, gitErr))
		}
		entries = append(entries, Entry{
			Repo:        repo,
			Slug:        filepath.Base(filepath.Dir(p)),
			RelPath:     rel,
			Name:        fm.Name,
			Description: fm.Description,
			Symptoms:    fm.Symptoms,
			Tags:        fm.Tags,
			AppliesTo:   fm.AppliesTo,
			MergedAt:    mergedAt,
		})
		return nil
	})
	if walkErr != nil {
		return nil, warnings, fmt.Errorf("skillserve: repo %s: walk %s: %w", repo, root, walkErr)
	}
	return entries, warnings, nil
}
