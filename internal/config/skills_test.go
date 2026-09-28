package config

// Skills and Skill Repo Table Tests
//
// Governing tests: SPEC-0007 REQ "Skill Repos" — the tables parse with their
// defaults, refusals name the offending key, the tables are global-only, and
// with no skill repo declared nothing is registered.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

func TestSkillRepoDefaults(t *testing.T) {
	cfg, err := Parse([]byte("[skill_repo.go-stack]\nremote = \"https://git.example.com/org/go-skills.git\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SkillRepoOrder) != 1 {
		t.Fatalf("want one repo, got %v", cfg.SkillRepoOrder)
	}
	r := cfg.SkillRepos["go-stack"]
	if r.Remote != "https://git.example.com/org/go-skills.git" {
		t.Fatalf("remote = %q", r.Remote)
	}
	if r.Path != core.DefaultSkillRepoPath {
		t.Fatalf("path default = %q", r.Path)
	}
	if !r.Public {
		t.Fatal("unset public must be treated as true")
	}
	if got := r.ServeToOrDefault(); len(got) != 1 || got[0] != "*" {
		t.Fatalf("serve_to default = %v", got)
	}
}

func TestSkillsTableDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte("[skills]\nsummary_max_chars = 800\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Skills.SummaryMaxCharsOrDefault(); got != 800 {
		t.Fatalf("summary_max_chars = %d", got)
	}
	if cfg.Skills.RetireGraceDaysOrDefault() != core.DefaultRetireGraceDays ||
		cfg.Skills.RetireWindowDaysOrDefault() != core.DefaultRetireWindowDays {
		t.Fatalf("day defaults not applied: %+v", cfg.Skills)
	}
}

func TestSkillRepoValidationRefusals(t *testing.T) {
	cases := map[string]string{
		"missing remote": "[skill_repo.x]\npath = \"skills\"\n",
		"empty serve_to": "[skill_repo.x]\nremote = \"https://x/y.git\"\nserve_to = []\n",
		"empty selector": "[skill_repo.x]\nremote = \"https://x/y.git\"\nserve_to = [\"\"]\n",
		"empty path":     "[skill_repo.x]\nremote = \"https://x/y.git\"\npath = \"\"\n",
		"slash in name":  "[skill_repo.\"a/b\"]\nremote = \"https://x/y.git\"\n",
		"unknown key":    "[skill_repo.x]\nremote = \"https://x/y.git\"\nbogus = 1\n",
		"summary range":  "[skills]\nsummary_max_chars = 10\n",
		"window < grace": "[skills]\nretire_grace_days = 30\nretire_window_days = 10\n",
		"negative grace": "[skills]\nretire_grace_days = -1\n",
	}
	for name, doc := range cases {
		if _, err := Parse([]byte(doc), "t.toml"); err == nil {
			t.Errorf("%s: want a refusal, got none", name)
		}
	}
}

func TestSkillRepoGlobalOnly(t *testing.T) {
	// A project harness.toml cannot declare skill repos or the [skills]
	// table (SPEC-0007 REQ "Skill Repos": global configuration only).
	_, err := ParseProject([]byte("[skill_repo.go-stack]\nremote = \"https://x/y.git\"\n"), "harness.toml")
	if err == nil || !strings.Contains(err.Error(), "skill_repo") {
		t.Fatalf("project [skill_repo.*] must be refused, got %v", err)
	}
	_, err = ParseProject([]byte("[skills]\nsummary_max_chars = 500\n"), "harness.toml")
	if err == nil || !strings.Contains(err.Error(), "skills") {
		t.Fatalf("project [skills] must be refused, got %v", err)
	}
}

func TestNoSkillRepoDeclared(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.OrderedSkillRepos()) != 0 {
		t.Fatal("no repos should be declared")
	}
}
