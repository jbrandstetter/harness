// Governing: ADR-0040; SPEC-0026 REQ-3 (manifest schema), REQ-7 (source
// grammar), Error Handling Standards.
package agentpkg

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	ok := "stump-wtf/pr-reviewer@0123456789abcdef0123456789abcdef01234567"
	src, err := ParseSource(ok)
	if err != nil {
		t.Fatalf("parse %q: %v", ok, err)
	}
	if src.String() != ok {
		t.Errorf("round trip: got %q", src.String())
	}
	if src.Stable != "stump-wtf" || src.Package != "pr-reviewer" || src.SHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("fields: %+v", src)
	}

	for _, bad := range []string{
		"",                      // empty
		"stump-wtf/pr-reviewer", // no pin
		"stump-wtf@abc123",      // no package
		"stump-wtf/pr@main",     // branch, not sha
		"stump-wtf/pr@0123",     // short sha
		"Stump/pr@0123456789abcdef0123456789abcdef01234567",     // uppercase stable
		"stump-wtf/PR@0123456789abcdef0123456789abcdef01234567", // uppercase package
		"stump_wtf/pr@0123456789abcdef0123456789abcdef01234567", // underscore
	} {
		if _, err := ParseSource(bad); !errors.Is(err, ErrInvalidSource) {
			t.Errorf("ParseSource(%q): want ErrInvalidSource, got %v", bad, err)
		}
	}
}

const validManifest = `
[package]
name = "pr-reviewer"
version = "1.0.0"
description = "Reviews pull requests"

[harness]
harness = "claude-code"
model = "opus"
auto_accept = true

[requests]
skill_paths = true
mcp_allow = ["read"]
`

func TestParseManifestValid(t *testing.T) {
	m, err := ParseManifest([]byte(validManifest), "package.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.Package.Name != "pr-reviewer" || m.Package.Version != "1.0.0" {
		t.Errorf("package: %+v", m.Package)
	}
	if m.Harness.Harness != "claude-code" || m.Harness.Model != "opus" {
		t.Errorf("harness: %+v", m.Harness)
	}
	if m.Harness.AutoAccept == nil || !*m.Harness.AutoAccept {
		t.Errorf("auto_accept: %v", m.Harness.AutoAccept)
	}
	if m.Requests.SkillPaths == nil || !*m.Requests.SkillPaths {
		t.Errorf("skill_paths: %v", m.Requests.SkillPaths)
	}
	if len(m.Requests.MCPAllow) != 1 || m.Requests.MCPAllow[0] != "read" {
		t.Errorf("mcp_allow: %v", m.Requests.MCPAllow)
	}
}

func TestParseManifestViolations(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name:    "unknown table",
			src:     "[harness]\nharness = \"crush\"\n[adapter.claude-code]\nfoo = 1\n",
			wantErr: "[adapter]",
		},
		{
			name:    "env_file rejected by name",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\nenv_file = \"~/.vault/secrets.env\"\n",
			wantErr: "env_file",
		},
		{
			name:    "secrets_env rejected",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\nsecrets_env = \"TOKEN\"\n",
			wantErr: "secrets_env",
		},
		{
			name:    "schedule rejected",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\nschedule = \"@daily\"\n",
			wantErr: "schedule",
		},
		{
			name:    "triggers rejected",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\ntriggers = [\"webhook.foo\"]\n",
			wantErr: "triggers",
		},
		{
			name:    "secret reference rejected",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\nargs = [\"${HOME}/run\"]\n",
			wantErr: `"${"`,
		},
		{
			name:    "secret reference in model",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\nmodel = \"${MODEL}\"\n",
			wantErr: "model",
		},
		{
			name:    "unknown request key",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\n[requests]\nfilesystem = true\n",
			wantErr: "filesystem",
		},
		{
			name:    "bad mcp_allow value",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\n[requests]\nmcp_allow = [\"admin\"]\n",
			wantErr: "mcp_allow",
		},
		{
			name:    "unknown harness allowlist key",
			src:     "[package]\nname = \"x\"\n[harness]\nharness = \"crush\"\nmodel_pin = \"opus\"\n",
			wantErr: "model_pin",
		},
		{
			name:    "unknown package key",
			src:     "[package]\nname = \"x\"\nlicense = \"MIT\"\n[harness]\nharness = \"crush\"\n",
			wantErr: "license",
		},
		{
			name:    "missing package name",
			src:     "[package]\nversion = \"1\"\n[harness]\nharness = \"crush\"\n",
			wantErr: "name",
		},
		{
			name:    "missing harness table",
			src:     "[package]\nname = \"x\"\n",
			wantErr: "[harness]",
		},
		{
			name:    "missing adapter",
			src:     "[package]\nname = \"x\"\n[harness]\nmodel = \"opus\"\n",
			wantErr: "harness",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tt.src), "package.toml")
			if !errors.Is(err, ErrManifestViolation) {
				t.Fatalf("want ErrManifestViolation, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestLoadManifestMissingFile verifies a read of a nonexistent pin surfaces
// the fs error config load translates into the missing-pin message.
func TestLoadManifestMissingFile(t *testing.T) {
	_, err := LoadManifest(filepath.Join(t.TempDir(), "nope", "package.toml"))
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}
