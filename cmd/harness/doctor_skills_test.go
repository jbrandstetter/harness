// Doctor: skill repo serving clone checks.
//
// Governing tests: SPEC-0007 REQ "Default-Branch Gate" — a detached or dirty
// clone warns with an actionable hint, a healthy fleet passes, and a config
// with no skill repo emits no row.
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package main

import (
	"errors"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

type stubSkillsClient struct {
	data protocol.SkillsStatusData
	err  error
}

func (s stubSkillsClient) SkillsStatus() (protocol.SkillsStatusData, error) {
	return s.data, s.err
}

func skillConfig() *core.Config {
	return &core.Config{
		SkillRepos: map[string]core.SkillRepo{
			"go-stack": {Name: "go-stack", Remote: "https://git.example.com/org/go.git", ServeTo: []string{"*"}, Public: true},
		},
		SkillRepoOrder: []string{"go-stack"},
	}
}

func TestDoctorSkillsHealthy(t *testing.T) {
	r := skillsCheck(skillConfig(), stubSkillsClient{data: protocol.SkillsStatusData{Repos: []protocol.SkillRepoStatus{
		{Name: "go-stack", State: "ok", Skills: 2},
	}}})
	if r == nil || r.level != cliui.LevelSuccess {
		t.Fatalf("want a passing row, got %+v", r)
	}
}

func TestDoctorSkillsGateWarnings(t *testing.T) {
	for _, tc := range []struct {
		state  string
		detail string
	}{
		{"detached", "HEAD is on \"proposal/x\", not the default branch \"main\""},
		{"dirty", "the clone's working tree is dirty"},
		{"missing", ""},
	} {
		r := skillsCheck(skillConfig(), stubSkillsClient{data: protocol.SkillsStatusData{Repos: []protocol.SkillRepoStatus{
			{Name: "go-stack", State: tc.state, Detail: tc.detail},
		}}})
		if r == nil || r.level != cliui.LevelWarn {
			t.Fatalf("%s: want a warning row, got %+v", tc.state, r)
		}
		if r.hint == "" {
			t.Fatalf("%s: warning must carry an actionable hint", tc.state)
		}
	}
}

func TestDoctorSkillsMalformedFileWarning(t *testing.T) {
	r := skillsCheck(skillConfig(), stubSkillsClient{data: protocol.SkillsStatusData{Repos: []protocol.SkillRepoStatus{
		{Name: "go-stack", State: "ok", Skills: 1,
			Warnings: []string{"skills/bad/SKILL.md: name is required"}},
	}}})
	if r == nil || r.level != cliui.LevelWarn {
		t.Fatalf("a malformed skill must warn, got %+v", r)
	}
}

func TestDoctorSkillsNoReposNoRow(t *testing.T) {
	if r := skillsCheck(nil, stubSkillsClient{}); r != nil {
		t.Fatalf("no skill repos: no row, got %+v", r)
	}
	cfg := &core.Config{}
	if r := skillsCheck(cfg, stubSkillsClient{}); r != nil {
		t.Fatalf("empty config: no row, got %+v", r)
	}
}

func TestDoctorSkillsDaemonUnreachableIsNotEvidence(t *testing.T) {
	if r := skillsCheck(skillConfig(), stubSkillsClient{err: errors.New("dial failed")}); r != nil {
		t.Fatalf("an unanswered daemon is not evidence a clone is broken, got %+v", r)
	}
}
