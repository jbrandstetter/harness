// Package skillmerge is the SPEC-0006 engine: it merges a harness's skill
// roots in fixed precedence order, attributes every name collision to the
// winning root with the shadowed copies enumerable, and projects the merged
// set into the adapter's target directory by copy — never symlink — before
// the harness's command executes.
//
// The precedence is the caller's to assemble: package bundle (lowest, when
// SPEC-0026 REQ-10's tier lands on this engine), adapter defaults, global
// skill_paths, project skill_paths, project-local directories (highest).
// Each Root carries its tier; ties break toward the later root.
//
// Governing: SPEC-0006 REQ "Ordered Merge and Shadowing", "Spawn-Time
// Projection", "Skill Path Configuration", "Error Handling Standards";
// ADR-0011 (projection happens at spawn, copy not symlink).
//
// @joestump-agent 10/04/2026 - Added for harness#75.
package skillmerge

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// SkillSlug is the file that marks a directory as a skill (ADR-0011's
// skills/<slug>/SKILL.md shape).
const SkillSlug = "SKILL.md"

// Sentinel errors callers distinguish (SPEC-0006 REQ "Error Handling
// Standards").
var (
	// ErrTargetNotWritable is wrapped by every failure to write the
	// projection target; the error names the adapter, the target path and
	// the underlying cause.
	ErrTargetNotWritable = errors.New("projection target not writable")
)

// Root is one contributing skill directory at one precedence tier.
type Root struct {
	// Dir is the absolute directory whose immediate children are skill
	// directories.
	Dir string
	// Tier is the precedence: higher wins a name collision. Callers number
	// the tiers; the engine only compares.
	Tier int
	// Source names the tier for diagnostics ("adapter default", "global
	// skill_paths", "project skill_paths", "project local", …).
	Source string
}

// Skill is one resolved skill name: the winning root and every shadowed
// one, attributable on demand (SPEC-0006 "Shadowed copies MUST remain
// enumerable through a diagnostic surface").
type Skill struct {
	Name     string
	Winner   string // directory holding the winning copy
	WinSrc   string // that root's Source label
	Shadowed []ShadowedCopy
}

// ShadowedCopy is one losing copy of a skill name.
type ShadowedCopy struct {
	Dir    string
	Source string
	Tier   int
}

// ResolveWarning reports a root the merge could not read: the harness still
// starts with the remaining roots merged (SPEC-0006 "An unreadable skill
// root does not block startup").
type ResolveWarning struct {
	Dir string
	Err error
}

func (w ResolveWarning) Error() string {
	return fmt.Sprintf("skill root %s: %v", w.Dir, w.Err)
}

// Resolve merges roots into the resolved skill set, highest tier winning —
// ties break toward the later root in the caller's list. A directory
// contributes a skill only when it directly holds a SKILL.md; deeper trees
// are the skill's own business. An unreadable root becomes a warning, never
// an error (SPEC-0006: "An unreadable skill root does not block startup").
// The returned map is keyed by skill name and deterministic.
func Resolve(roots []Root) (map[string]Skill, []ResolveWarning) {
	type copyRef struct {
		dir    string
		source string
		tier   int
		order  int
	}
	copies := map[string][]copyRef{}
	var warnings []ResolveWarning
	for order, r := range roots {
		entries, err := os.ReadDir(r.Dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // a root that does not exist contributes nothing
			}
			warnings = append(warnings, ResolveWarning{Dir: r.Dir, Err: err})
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(r.Dir, e.Name(), SkillSlug)); err != nil {
				continue // not a skill directory
			}
			copies[e.Name()] = append(copies[e.Name()], copyRef{
				dir:    filepath.Join(r.Dir, e.Name()),
				source: r.Source,
				tier:   r.Tier,
				order:  order,
			})
		}
	}

	set := make(map[string]Skill, len(copies))
	for name, list := range copies {
		winner := list[0]
		var others []copyRef
		for _, c := range list[1:] {
			if c.tier > winner.tier || (c.tier == winner.tier && c.order > winner.order) {
				others = append(others, winner)
				winner = c
				continue
			}
			others = append(others, c)
		}
		sort.Slice(others, func(i, j int) bool {
			if others[i].tier != others[j].tier {
				return others[i].tier < others[j].tier
			}
			return others[i].order < others[j].order
		})
		skill := Skill{Name: name, Winner: winner.dir, WinSrc: winner.source}
		for _, o := range others {
			skill.Shadowed = append(skill.Shadowed, ShadowedCopy{Dir: o.dir, Source: o.source, Tier: o.tier})
		}
		set[name] = skill
	}
	return set, warnings
}

// Project materializes the merged set into target by copying each winning
// skill directory's contents — never a symlink into a source root, so a
// harness writing into its own projected skill directory cannot mutate a
// shared source (SPEC-0006 "Projection is by copy, not link"). Existing
// directories in the target that the merge did not produce are the tool's
// own concern and are left untouched; the merge never deletes what it does
// not own. Every error wraps ErrTargetNotWritable and names the target.
func Project(target string, set map[string]Skill) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrTargetNotWritable, target, err)
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dst := filepath.Join(target, name)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("%w: %s: create %s: %v", ErrTargetNotWritable, target, dst, err)
		}
		if err := copyDir(dst, set[name].Winner); err != nil {
			return fmt.Errorf("%w: %s: copy %s: %v", ErrTargetNotWritable, target, set[name].Winner, err)
		}
	}
	return nil
}

// copyDir copies src's regular file tree into dst. A symlink inside a
// source skill is refused: a link out of the projected tree is exactly the
// mutation channel the copy rule exists to close, and the ADR-0011 skill
// shape has no need for one.
func copyDir(dst, src string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink %s: projection is by copy only", s)
		}
		if e.IsDir() {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
			if err := copyDir(d, s); err != nil {
				return err
			}
			continue
		}
		raw, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		if err := os.WriteFile(d, raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}
