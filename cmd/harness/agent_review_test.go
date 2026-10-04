package main

// The Upgrade Effective-Value Review (issue #882)
//
// REQ-8 as amended: a diff between the harness's current effective values
// and the candidate never auto-applies. --yes and unattended runs refuse
// loudly naming the changes; an interactive run chooses per change, and the
// choice is written — kept package values pinned onto the table, taken new
// values written, taken rows over local overrides removing them.
//
// Governing: ADR-0044, SPEC-0026 REQ-8, REQ-4 (issue #882).
//
// @joestump-agent 10/02/2026 - Added for harness#882.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/config/tomledit"
	"github.com/stump-wtf/harness/internal/core"
)

func reviewCmd(buf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd
}

func changeFor(t *testing.T, chgs []agentpkg.EffectiveChange, key string) agentpkg.EffectiveChange {
	t.Helper()
	for _, c := range chgs {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("no change for key %q in %v", key, chgs)
	return agentpkg.EffectiveChange{}
}

// The review rows are exactly the behavioral ones: a moved package value,
// a dropped one, a new key, a contradicted local override — and never a
// local override the new pin simply does not mention, or unchanged values,
// or package metadata.
func TestEffectiveChanges(t *testing.T) {
	h := &core.Harness{
		Name:             "pr-reviewer",
		Adapter:          "claude-code",
		Model:            "pkg-model",
		SystemPromptFile: "/pin/system.md",
		MCPAllow:         []string{"read"},
		PackageKeys:      []string{"harness", "model", "system_prompt_file"},
	}
	newMan := &agentpkg.Manifest{
		Package: agentpkg.PackageMeta{Version: "9.9.9"},
		Harness: agentpkg.HarnessValues{
			Harness:          "claude-code",
			Model:            "newer-model",
			Args:             []string{"--deep"},
			SystemPromptFile: "",
			MCPConfig:        "/pin/mcp.json",
		},
		Requests: agentpkg.Requests{MCPAllow: []string{"read", "write"}},
	}
	chgs := agentpkg.EffectiveChanges(h, newMan)

	// model moved with the pin (package-supplied).
	c := changeFor(t, chgs, "model")
	if c.Old != "pkg-model" || c.New != "newer-model" || c.OldLocal || c.Added {
		t.Fatalf("model change shape wrong: %+v", c)
	}
	// args is new with this version.
	c = changeFor(t, chgs, "args")
	if !c.Added || c.Old != nil {
		t.Fatalf("an introduced key must be marked added: %+v", c)
	}
	// system_prompt_file is dropped by the new pin.
	c = changeFor(t, chgs, "system_prompt_file")
	if c.Old == nil || c.New != nil || c.OldLocal {
		t.Fatalf("a dropped package value must be a removal row: %+v", c)
	}
	// mcp_config is new.
	changeFor(t, chgs, "mcp_config")
	// the requested write scope is not granted.
	c = changeFor(t, chgs, "mcp_allow")
	if c.Kind != "request" || c.Render(c.New) != "[read, write]" {
		t.Fatalf("the ungranted scope must be a request row: %+v", c)
	}
	// harness is unchanged; version metadata never appears.
	for _, c := range chgs {
		if c.Key == "harness" || c.Key == "version" {
			t.Fatalf("unchanged or metadata key must not be reviewed: %+v", c)
		}
	}

	// A local override the new pin contradicts is a conflict row; one the
	// pin does not mention is not a row at all.
	h2 := &core.Harness{
		Adapter:     "claude-code",
		Model:       "my-own",
		Workdir:     "/srv",
		PackageKeys: []string{"harness"},
	}
	chgs = agentpkg.EffectiveChanges(h2, newMan)
	c = changeFor(t, chgs, "model")
	if !c.OldLocal || c.Old != "my-own" || c.New != "newer-model" {
		t.Fatalf("a contradicted override must be a conflict row: %+v", c)
	}
	for _, c := range chgs {
		if c.Key == "workdir" {
			t.Fatalf("an override the pin never mentions is not a row: %+v", c)
		}
	}

	// No behavioral difference: no rows at all.
	same := &core.Harness{Adapter: "claude-code", PackageKeys: []string{"harness"}}
	sameMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code"}}
	if chgs := agentpkg.EffectiveChanges(same, sameMan); len(chgs) != 0 {
		t.Fatalf("no-op upgrade must have no review rows: %v", chgs)
	}
}

// The choice parser: Enter keeps everything, all takes everything, a comma
// list takes those rows, and garbage refuses.
func TestParseReviewChoice(t *testing.T) {
	take, err := parseReviewChoice("", 3)
	if err != nil || len(take) != 0 {
		t.Fatalf("empty answer keeps all, got %v %v", take, err)
	}
	take, err = parseReviewChoice("all", 3)
	if err != nil || len(take) != 3 {
		t.Fatalf("all takes all, got %v %v", take, err)
	}
	take, err = parseReviewChoice("1, 3", 3)
	if err != nil || len(take) != 2 || !take[0] || !take[2] {
		t.Fatalf("comma list takes those rows, got %v %v", take, err)
	}
	if _, err := parseReviewChoice("9", 3); err == nil {
		t.Fatal("an out-of-range row must refuse")
	}
	if _, err := parseReviewChoice("yes", 3); err == nil {
		t.Fatal("a non-numeric answer must refuse")
	}
}

// The apply mapping, on the real editor: keep pins the old value onto the
// table, take removes the local override, a taken request grants the scope.
func TestApplyReviewChoices(t *testing.T) {
	src := `[harness.reviewer]
model = "my-own"
args = ["--strict"]
source = "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000"
`
	ed := tomledit.New([]byte(src))
	h := core.Harness{
		Adapter:  "claude-code",
		Model:    "my-own",
		Args:     []string{"--strict"},
		MCPAllow: []string{"read"},
	}
	newMan := &agentpkg.Manifest{
		Harness:  agentpkg.HarnessValues{Harness: "claude-code", Model: "newer-model"},
		Requests: agentpkg.Requests{MCPAllow: []string{"read", "write"}},
	}
	// model: take the package's (drop the local override);
	// args: keep the local override;
	// mcp_allow: grant the requested scope.
	take := map[int]bool{}
	chgs := agentpkg.EffectiveChanges(&h, newMan)
	for i, c := range chgs {
		switch c.Key {
		case "model":
			take[i] = true
		case "args":
			// not taken (it is not a row at all: the new pin has no args)
		case "mcp_allow":
			take[i] = true
		}
	}
	var out bytes.Buffer
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", h, newMan, take); err != nil {
		t.Fatal(err)
	}
	data := string(ed.Bytes())
	if !strings.Contains(data, `source = "stump-wtf/pr-reviewer@`) {
		t.Fatalf("source untouched:\n%s", data)
	}
	if strings.Contains(data, "model") {
		t.Fatalf("taking the package's model must remove the local override:\n%s", data)
	}
	if !strings.Contains(data, `args = ["--strict"]`) {
		t.Fatalf("the untouched local override must stay byte-identical:\n%s", data)
	}
	if !strings.Contains(data, `mcp_allow = ["read", "write"]`) {
		t.Fatalf("the granted scope must be written:\n%s", data)
	}
}

// Keeping a package-supplied value pins it onto the table as an explicit
// override, so the choice is real on disk.
func TestApplyReviewChoicesKeepsPinned(t *testing.T) {
	src := `[harness.reviewer]
source = "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000"
`
	ed := tomledit.New([]byte(src))
	h := core.Harness{
		Adapter:     "claude-code",
		Model:       "pkg-model",
		PackageKeys: []string{"harness", "model"},
	}
	newMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", Model: "newer-model"}}
	chgs := agentpkg.EffectiveChanges(&h, newMan)
	if len(chgs) != 1 {
		t.Fatalf("want the one model row, got %v", chgs)
	}
	var out bytes.Buffer
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", h, newMan, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ed.Bytes()), `model = "pkg-model"`) {
		t.Fatalf("keeping must pin the old value:\n%s", ed.Bytes())
	}
}

// --yes bombs out loudly on a reviewable diff: a moved package value
// refuses the upgrade instead of auto-applying, and the source is
// untouched. The same refusal answers an unattended run without --yes.
func TestAgentUpgradeReviewRefusesUnderYes(t *testing.T) {
	e := newAgentEnv(t)
	v1 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
args = ["--deep"]`, 1)
	remote, work := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": v1})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

	// The new pin moves the args.
	v2 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
args = ["--deeper"]`, 1)
	if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	agentGit(t, work, "add", "-A")
	agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "move args")
	agentGit(t, work, "push", "-q", remote, "main")
	if _, _, err := e.run("agent", "stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}

	_, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err == nil || !strings.Contains(err.Error(), "may want to review") || !strings.Contains(err.Error(), "args: [--deep] -> [--deeper]") {
		t.Fatalf("--yes must refuse a reviewable diff naming it, got %v", err)
	}
	// Unattended without --yes refuses the same way.
	_, _, err = e.run("agent", "upgrade", "stump-wtf/pr-reviewer")
	if err == nil || !strings.Contains(err.Error(), "may want to review") {
		t.Fatalf("an unattended run must refuse a reviewable diff, got %v", err)
	}
	if src := installedSource(t, e.cfgPath, "pr-reviewer"); src.SHA != oldSrc.SHA {
		t.Fatalf("a refused upgrade must not move the source: %v -> %v", oldSrc.SHA, src.SHA)
	}
}

// Install writes the confirmed requested scope onto the table (issue #882):
// a package declaring scopes beyond the default lands them visibly.
func TestAgentInstallWritesConfirmedScope(t *testing.T) {
	e := newAgentEnv(t)
	manifest := strings.Replace(agentPkg, `[harness]
harness = "claude-code"`, `[harness]
harness = "claude-code"

[requests]
mcp_allow = ["read"]`, 1)
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": manifest})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `mcp_allow = ["read"]`) {
		t.Fatalf("the confirmed scope must be written onto the table:\n%s", data)
	}
	// And it loads back as the effective grant.
	cfg, err := loadGlobalConfig(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Harnesses["pr-reviewer"].MCPAllow; len(got) != 1 || got[0] != "read" {
		t.Fatalf("the grant must be effective after reload: %v", got)
	}
}
