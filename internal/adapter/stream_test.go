package adapter

// Governing tests: ADR-0033 "Normalization belongs to agent-trace" (an
// adapter declares its prompt stream format); SPEC-0006 REQ "Structured
// Prompt Stream".

import (
	"slices"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// Only claude-code declares a stream. Every other adapter keeps its PTY: a
// declaration it does not back with its argv would put a terminal program on
// pipes.
func TestOnlyClaudeCodeDeclaresAPromptStream(t *testing.T) {
	r := NewRegistry()
	for _, name := range r.Names() {
		a, err := r.Get(name)
		if err != nil {
			t.Fatalf("Get(%q): %v", name, err)
		}
		got := PromptStreamOf(a)
		want := StreamFormat("")
		if name == "claude-code" {
			want = StreamJSON
		}
		if got != want {
			t.Errorf("%s declares prompt stream %q, want %q", name, got, want)
		}
	}
}

// A declared format is the one the argv asks for. If PromptCommand ever stops
// passing it, the declaration would put a terminal-mode CLI on pipes, so the
// two are pinned together here for every adapter that declares one.
func TestDeclaredPromptStreamMatchesTheArgv(t *testing.T) {
	r := NewRegistry()
	for _, name := range r.Names() {
		a, _ := r.Get(name)
		format := PromptStreamOf(a)
		if format == "" {
			continue
		}
		for _, opts := range []core.AgentOpts{{}, {Model: "m", AutoAccept: true, MaxTurns: 3, AllowedTools: []string{"Read"}}} {
			_, args := a.PromptCommand("do the thing", opts)
			i := slices.Index(args, "--output-format")
			if i < 0 || i+1 >= len(args) || args[i+1] != string(format) {
				t.Errorf("%s declares %q but its argv is %q", name, format, args)
			}
		}
	}
}
